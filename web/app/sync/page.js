"use client";

import { useCallback, useEffect, useState } from "react";

const STATUSES = ["failed", "pending", "pushed"];

function rupees(paise) {
  return "₹" + (paise / 100).toLocaleString("en-IN", { minimumFractionDigits: 2 });
}

export default function SyncPage() {
  const [vouchers, setVouchers] = useState([]);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState({}); // voucher id -> true while retrying

  const load = useCallback(async () => {
    try {
      const res = await fetch("/backend/vouchers", { cache: "no-store" });
      if (!res.ok) throw new Error(`API returned ${res.status}`);
      setVouchers(await res.json());
      setError("");
    } catch (e) {
      setError("Could not load vouchers: " + e.message);
    }
  }, []);

  // Refresh every 2 seconds so you can watch the worker make progress.
  useEffect(() => {
    load();
    const t = setInterval(load, 2000);
    return () => clearInterval(t);
  }, [load]);

  async function retry(id) {
    setBusy((b) => ({ ...b, [id]: true }));
    try {
      const res = await fetch(`/backend/vouchers/${id}/retry`, { method: "POST" });
      if (!res.ok) {
        const body = await res.json().catch(() => ({}));
        throw new Error(body.error || `API returned ${res.status}`);
      }
      await load();
    } catch (e) {
      setError(`Retry of voucher ${id} failed: ${e.message}`);
    } finally {
      setBusy((b) => ({ ...b, [id]: false }));
    }
  }

  return (
    <main>
      <h1>Voucher sync</h1>
      {error && <p style={{ color: "#b00020" }}>{error}</p>}

      {STATUSES.map((status) => {
        const rows = vouchers.filter((v) => v.status === status);
        return (
          <section key={status} style={{ marginBottom: 32 }}>
            <h2 style={{ textTransform: "capitalize" }}>
              {status} ({rows.length})
            </h2>
            {rows.length === 0 ? (
              <p style={{ color: "#777" }}>None.</p>
            ) : (
              <table cellPadding={6} style={{ borderCollapse: "collapse", width: "100%" }}>
                <thead>
                  <tr style={{ textAlign: "left", borderBottom: "1px solid #ccc" }}>
                    <th>ID</th>
                    <th>Source</th>
                    <th>Amount</th>
                    <th>Attempts</th>
                    <th>Last error</th>
                    <th>External ref</th>
                    <th>Updated</th>
                    <th></th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((v) => (
                    <tr key={v.id} style={{ borderBottom: "1px solid #eee" }}>
                      <td>{v.id}</td>
                      <td>
                        {v.type} #{v.source_id}
                      </td>
                      <td>{rupees(v.amount_paise)}</td>
                      <td>{v.attempts}</td>
                      <td style={{ color: "#b00020" }}>{v.last_error || "—"}</td>
                      <td>{v.external_ref || "—"}</td>
                      <td>{new Date(v.updated_at).toLocaleTimeString()}</td>
                      <td>
                        {v.status === "failed" && (
                          <button onClick={() => retry(v.id)} disabled={busy[v.id]}>
                            {busy[v.id] ? "Retrying…" : "Retry"}
                          </button>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </section>
        );
      })}
    </main>
  );
}
