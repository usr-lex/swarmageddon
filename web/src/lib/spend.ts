import type { SpendRow } from "@/api/types";

export interface SpendGroup {
  key: string;
  usd: number;
  budgetUSD: number;
  inputTokens: number;
  outputTokens: number;
}

/** Sums spend rows by room, workspace or model, largest first. */
export function groupSpend(rows: SpendRow[], by: "room" | "workspace" | "model"): SpendGroup[] {
  const m = new Map<string, SpendGroup>();
  for (const r of rows) {
    const key = by === "room" ? `${r.room}@${r.workspace}` : by === "workspace" ? r.workspace : r.model;
    const g = m.get(key) ?? { key, usd: 0, budgetUSD: 0, inputTokens: 0, outputTokens: 0 };
    g.usd += r.usd;
    // Budgets are per room; a model's budget isn't meaningful.
    if (by !== "model") g.budgetUSD += r.budgetUSD;
    g.inputTokens += r.inputTokens;
    g.outputTokens += r.outputTokens;
    m.set(key, g);
  }
  return [...m.values()].sort((a, b) => b.usd - a.usd || a.key.localeCompare(b.key));
}
