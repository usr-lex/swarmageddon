import type { SpendReport } from "@/api/types";

export interface SpendGroup {
  key: string;
  usd: number;
  /** 0 when there's no budget (models have none). */
  budgetUSD: number;
  exhausted: boolean;
}

/** Spend by room, workspace or model, largest first. */
export function groupSpend(r: SpendReport, by: "room" | "workspace" | "model"): SpendGroup[] {
  let out: SpendGroup[];
  if (by === "room") {
    out = r.rooms.map((x) => ({ key: `${x.room}@${x.workspace}`, usd: x.usd, budgetUSD: x.budgetUSD, exhausted: x.exhausted }));
  } else if (by === "workspace") {
    out = r.workspaces.map((w) => {
      const rooms = r.rooms.filter((x) => x.workspace === w.workspace);
      return { key: w.workspace, usd: w.usd, budgetUSD: rooms.reduce((a, x) => a + x.budgetUSD, 0), exhausted: false };
    });
  } else {
    const m = new Map<string, number>();
    for (const w of r.workspaces) for (const x of w.models) m.set(x.model, (m.get(x.model) ?? 0) + x.usd);
    out = [...m.entries()].map(([key, usd]) => ({ key, usd, budgetUSD: 0, exhausted: false }));
  }
  return out.sort((a, b) => b.usd - a.usd || a.key.localeCompare(b.key));
}
