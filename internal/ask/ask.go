// Package ask answers questions like "how much of SKU X is still with
// job-worker Y?" using an LLM (via OpenRouter) that can only call read-only
// database tools. Every tool call is logged and returned in the response.
//
// With no OPENROUTER_API_KEY, a stub "model" is used instead: it picks the
// SKU and job-worker by simple name matching, calls the same tools, and fills
// in a fixed sentence. The response says model "stub" so this is never hidden.
package ask

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const maxModelTurns = 6 // stop runaway tool loops

type Config struct {
	APIKey  string // OPENROUTER_API_KEY; empty means use the stub
	Model   string // e.g. anthropic/claude-haiku-5.5
	BaseURL string // https://openrouter.ai/api/v1
}

type Service struct {
	cfg    Config
	tools  tools
	client *http.Client
	log    *slog.Logger
}

func NewService(cfg Config, pool *pgxpool.Pool, log *slog.Logger) *Service {
	return &Service{
		cfg:    cfg,
		tools:  tools{pool: pool},
		client: &http.Client{Timeout: 45 * time.Second},
		log:    log,
	}
}

type ToolCall struct {
	Name     string          `json:"name"`
	Args     json.RawMessage `json:"args"`
	Result   json.RawMessage `json:"result,omitempty"`
	Error    string          `json:"error,omitempty"`
	Duration string          `json:"duration"`
}

type Answer struct {
	Answer    string     `json:"answer"`
	Model     string     `json:"model"` // "stub" when no API key is set
	ToolCalls []ToolCall `json:"tool_calls"`
}

// Ask runs the question through the model (or the stub).
func (s *Service) Ask(ctx context.Context, question string) (Answer, error) {
	if s.cfg.APIKey == "" {
		return s.askStub(ctx, question)
	}
	return s.askLLM(ctx, question)
}

// callTool runs one tool, logs it, and records it for the response.
func (s *Service) callTool(ctx context.Context, name, args string, calls *[]ToolCall) string {
	start := time.Now()
	result, err := s.tools.run(ctx, name, args)
	tc := ToolCall{Name: name, Args: rawOrEmpty(args), Duration: time.Since(start).Round(time.Millisecond).String()}
	if err != nil {
		tc.Error = err.Error()
		result = fmt.Sprintf(`{"error": %q}`, err.Error())
	} else {
		tc.Result = json.RawMessage(result)
	}
	*calls = append(*calls, tc)
	s.log.Info("ask tool call", "tool", name, "args", args, "error", tc.Error, "duration", tc.Duration)
	return result
}

func rawOrEmpty(s string) json.RawMessage {
	if s == "" || !json.Valid([]byte(s)) {
		return json.RawMessage("{}")
	}
	return json.RawMessage(s)
}

// --- OpenRouter (OpenAI-compatible chat completions with tools) ---

const systemPrompt = `You answer questions for a women's ethnicwear studio about fabric sent out to job-workers (embroidery and stitching units).
"Still with the job-worker" means outstanding = issued - received.
Always use the tools to look up SKUs, job-workers and quantities; never guess numbers.
If a name matches several SKUs or job-workers, say so and list them instead of picking one.
Answer in one or two short sentences with the number of pieces.`

type message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func (s *Service) askLLM(ctx context.Context, question string) (Answer, error) {
	ans := Answer{Model: s.cfg.Model, ToolCalls: []ToolCall{}}
	msgs := []message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: question},
	}

	for range maxModelTurns {
		reply, err := s.chat(ctx, msgs)
		if err != nil {
			return ans, err
		}
		msgs = append(msgs, reply)

		if len(reply.ToolCalls) == 0 {
			ans.Answer = strings.TrimSpace(reply.Content)
			return ans, nil
		}
		for _, tc := range reply.ToolCalls {
			result := s.callTool(ctx, tc.Function.Name, tc.Function.Arguments, &ans.ToolCalls)
			msgs = append(msgs, message{Role: "tool", ToolCallID: tc.ID, Content: result})
		}
	}
	return ans, errors.New("model did not finish within the tool-call limit")
}

func (s *Service) chat(ctx context.Context, msgs []message) (message, error) {
	body, err := json.Marshal(map[string]any{
		"model":       s.cfg.Model,
		"messages":    msgs,
		"tools":       toolSpecs,
		"tool_choice": "auto",
		"temperature": 0,
	})
	if err != nil {
		return message{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return message{}, err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Title", "Sutra Mini")

	resp, err := s.client.Do(req)
	if err != nil {
		return message{}, fmt.Errorf("openrouter request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return message{}, fmt.Errorf("openrouter returned %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}

	var parsed struct {
		Choices []struct {
			Message message `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return message{}, fmt.Errorf("decode openrouter response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return message{}, fmt.Errorf("openrouter returned no choices: %s", truncate(string(raw), 300))
	}
	return parsed.Choices[0].Message, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
