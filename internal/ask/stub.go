package ask

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// askStub stands in for the LLM when no API key is configured. It follows the
// same path a model would (list SKUs, list job-workers, get outstanding), but
// chooses by plain word matching instead of understanding the question.
func (s *Service) askStub(ctx context.Context, question string) (Answer, error) {
	ans := Answer{Model: "stub", ToolCalls: []ToolCall{}}
	q := strings.ToLower(question)

	var skus []sku
	if err := json.Unmarshal([]byte(s.callTool(ctx, "find_skus", `{"query":""}`, &ans.ToolCalls)), &skus); err != nil {
		return ans, fmt.Errorf("stub: read skus: %w", err)
	}
	var parties []party
	if err := json.Unmarshal([]byte(s.callTool(ctx, "find_job_workers", `{"query":""}`, &ans.ToolCalls)), &parties); err != nil {
		return ans, fmt.Errorf("stub: read job-workers: %w", err)
	}

	bestSKU, skuScore := sku{}, 0
	for _, k := range skus {
		if score := matchScore(q, k.Code+" "+k.Name); score > skuScore {
			bestSKU, skuScore = k, score
		}
	}
	bestParty, partyScore := party{}, 0
	for _, p := range parties {
		if score := matchScore(q, p.Name); score > partyScore {
			bestParty, partyScore = p, score
		}
	}
	if skuScore == 0 || partyScore == 0 {
		ans.Answer = "[stub model] I couldn't find both a SKU and a job-worker name in the question. " +
			"Try e.g. \"How much of ANK-001 is still with Meena?\""
		return ans, nil
	}

	args := fmt.Sprintf(`{"sku_id":%d,"party_id":%d}`, bestSKU.ID, bestParty.ID)
	var out outstanding
	if err := json.Unmarshal([]byte(s.callTool(ctx, "get_outstanding", args, &ans.ToolCalls)), &out); err != nil {
		return ans, fmt.Errorf("stub: read outstanding: %w", err)
	}
	ans.Answer = fmt.Sprintf("[stub model] %d pieces of %s are still with %s (issued %d, received %d).",
		out.TotalOutstanding, out.SKU, out.JobWorker, out.TotalIssued, out.TotalReceived)
	return ans, nil
}

// matchScore counts how many meaningful words of name appear in the question.
// The SKU code (e.g. "ank-001") counts as a strong match.
func matchScore(question, name string) int {
	score := 0
	for _, w := range strings.Fields(strings.ToLower(name)) {
		if len(w) >= 4 && strings.Contains(question, w) {
			score++
			if strings.Contains(w, "-") {
				score += 5
			}
		}
	}
	return score
}
