import { useMemo } from "react";
import type { HeatCell } from "@/api/observatory";
import { Spinner } from "@/components/ui";
import { TipRow, useTip } from "./common";

const DAY = 86_400_000;

/** Five bins, one hue: 0 is the empty cell; the rest are quantiles of non-zero days. */
export function heatBins(counts: number[]): number[] {
  const nz = counts.filter((c) => c > 0).sort((a, b) => a - b);
  if (!nz.length) return [1, 2, 3, 4];
  const q = (p: number) => nz[Math.min(nz.length - 1, Math.floor(p * nz.length))];
  return [q(0.25), q(0.5), q(0.75), q(0.92)];
}

export function binOf(c: number, bins: number[]): number {
  if (c <= 0) return 0;
  let b = 1;
  for (const t of bins) if (c > t) b++;
  return Math.min(4, b);
}

const fills = [
  "var(--surface-2)",
  "color-mix(in srgb, var(--viz-1) 28%, var(--surface))",
  "color-mix(in srgb, var(--viz-1) 52%, var(--surface))",
  "color-mix(in srgb, var(--viz-1) 76%, var(--surface))",
  "var(--viz-1)",
];

interface Grid {
  weeks: { day: string; count: number }[][];
  months: { col: number; label: string }[];
}

/** Lays days out GitHub-style: a column per week (Sunday first), a row per weekday. */
export function calendar(byDay: Map<string, number>, days: number, end = Date.now()): Grid {
  const last = new Date(new Date(end).toISOString().slice(0, 10) + "T00:00:00Z").getTime();
  let t = last - (days - 1) * DAY;
  t -= new Date(t).getUTCDay() * DAY;
  const weeks: Grid["weeks"] = [];
  const months: Grid["months"] = [];
  let prevMonth = -1;
  for (; t <= last; t += 7 * DAY) {
    const col: { day: string; count: number }[] = [];
    for (let d = 0; d < 7; d++) {
      const tt = t + d * DAY;
      if (tt > last) break;
      const day = new Date(tt).toISOString().slice(0, 10);
      col.push({ day, count: byDay.get(day) ?? 0 });
    }
    const m = new Date(t).getUTCMonth();
    if (m !== prevMonth) {
      months.push({ col: weeks.length, label: new Date(t).toLocaleString("en", { month: "short", timeZone: "UTC" }) });
      prevMonth = m;
    }
    weeks.push(col);
  }
  return { weeks, months };
}

function Calendar({ byDay, days, cell, bins, name, tip, showMonths, maxScale }: { byDay: Map<string, number>; days: number; cell: number; bins: number[]; name: string; tip: ReturnType<typeof useTip>; showMonths?: boolean; maxScale?: number }) {
  const g = useMemo(() => calendar(byDay, days), [byDay, days]);
  const gap = cell > 6 ? 2 : 1;
  const top = showMonths ? 12 : 0;
  const w = g.weeks.length * (cell + gap);
  const h = top + 7 * (cell + gap);
  return (
    <svg viewBox={`0 0 ${w} ${h}`} width="100%" style={maxScale ? { maxWidth: w * maxScale } : undefined} role="img" aria-label={`Events per day, ${name}`}>
      {showMonths &&
        g.months.map((m) => (
          <text key={m.col} x={m.col * (cell + gap)} y={8} fontSize={8} className="fill-muted font-mono uppercase">
            {m.label}
          </text>
        ))}
      {g.weeks.map((col, x) =>
        col.map((d, y) => (
          <rect
            key={d.day}
            x={x * (cell + gap)}
            y={top + y * (cell + gap)}
            width={cell}
            height={cell}
            rx={cell > 6 ? 1.5 : 0.5}
            fill={fills[binOf(d.count, bins)]}
            onPointerMove={(e) => tip.show(e, <TipRow value={d.count} label={`events · ${name} · ${d.day}`} />)}
            onPointerLeave={tip.hide}
          />
        )),
      )}
    </svg>
  );
}

/** Contribution heatmap (§8.7): the whole swarm, then one small calendar per agent. */
export function Heatmap({ cells, days }: { cells?: HeatCell[]; days: number }) {
  const tip = useTip();
  const model = useMemo(() => {
    if (!cells) return null;
    const total = new Map<string, number>();
    const per = new Map<string, Map<string, number>>();
    for (const c of cells) {
      total.set(c.day, (total.get(c.day) ?? 0) + c.count);
      if (!per.has(c.agent)) per.set(c.agent, new Map());
      per.get(c.agent)!.set(c.day, c.count);
    }
    const sum = (m: Map<string, number>) => [...m.values()].reduce((a, b) => a + b, 0);
    return {
      total,
      totalBins: heatBins([...total.values()]),
      agentBins: heatBins(cells.map((c) => c.count)),
      agents: [...per.entries()].map(([name, m]) => ({ name, m, sum: sum(m) })).sort((a, b) => b.sum - a.sum),
      sum: sum(total),
    };
  }, [cells]);
  if (!model) return <Spinner label="Loading" />;
  return (
    <div ref={tip.ref} className="relative flex flex-col gap-3">
      <div>
        <div className="mb-1 flex items-baseline justify-between">
          <span className="label">Whole swarm</span>
          <span className="label tabular-nums">{model.sum.toLocaleString()} events</span>
        </div>
        <Calendar byDay={model.total} days={days} cell={10} bins={model.totalBins} name="swarm" tip={tip} showMonths maxScale={1.5} />
      </div>
      <div className="grid grid-cols-2 gap-x-5 gap-y-3 lg:grid-cols-3">
        {model.agents.map((a) => (
          <div key={a.name} className="min-w-0">
            <div className="flex items-baseline justify-between">
              <span className="font-mono text-[11px]">{a.name}</span>
              <span className="label tabular-nums">{a.sum}</span>
            </div>
            <Calendar byDay={a.m} days={days} cell={5} bins={model.agentBins} name={a.name} tip={tip} />
          </div>
        ))}
      </div>
      <div className="flex items-center gap-1 self-end" aria-hidden>
        <span className="label mr-1">Less</span>
        {fills.map((f, i) => (
          <svg key={i} width="10" height="10">
            <rect width="10" height="10" rx="1.5" fill={f} />
          </svg>
        ))}
        <span className="label ml-1">More</span>
      </div>
      {tip.node}
    </div>
  );
}
