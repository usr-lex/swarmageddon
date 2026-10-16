import { max } from "d3-array";
import { scaleBand, scaleLinear } from "d3-scale";
import { line } from "d3-shape";
import { useState, type ReactNode } from "react";
import type { ThroughputDay } from "@/api/observatory";
import { Spinner } from "@/components/ui";
import { usd } from "@/lib/format";
import { Legend, LegendItem, TipRow, useTip, useWidth } from "./common";

interface Props {
  days?: ThroughputDay[];
  /** agent -> group, for stack colors (one hue per group; segments separated by gaps). */
  groupOf: Map<string, string>;
  groups: string[];
}

const H = 64;
const M = { left: 34, right: 6, top: 6, bottom: 4 };

/** Throughput (§8.7): per day PRs merged and tokens stacked per agent, review latency, CI failures. Shared x, synced crosshair. */
export function Throughput({ days, groupOf, groups }: Props) {
  const [box, width] = useWidth<HTMLDivElement>(600);
  const tip = useTip();
  const [hover, setHover] = useState<number | null>(null);
  if (!days) return <Spinner label="Loading" />;

  const agents = [...groupOf.keys()].sort((a, b) => groups.indexOf(groupOf.get(a)!) - groups.indexOf(groupOf.get(b)!) || a.localeCompare(b));
  const color = (a: string) => (groups.indexOf(groupOf.get(a) ?? "") % 2 === 1 ? "var(--viz-2)" : "var(--viz-1)");
  const x = scaleBand<number>().domain(days.map((_, i) => i)).range([M.left, width - M.right]).paddingInner(0.2);
  const sum = (r: Record<string, number>) => Object.values(r).reduce((a, b) => a + b, 0);

  const onMove = (e: React.PointerEvent<SVGSVGElement>) => {
    const b = e.currentTarget.getBoundingClientRect();
    const px = ((e.clientX - b.left) / b.width) * width;
    const i = Math.max(0, Math.min(days.length - 1, Math.floor(((px - M.left) / (width - M.left - M.right)) * days.length)));
    setHover(i);
    const d = days[i];
    tip.show(
      e,
      <>
        <div className="label mb-0.5">{d.day}</div>
        <TipRow value={sum(d.prsMerged)} label="PRs merged" color="var(--viz-1)" />
        <TipRow value={d.reviewLatencyHours ?? "–"} label="h median review latency" color="var(--fg)" />
        <TipRow value={d.ciFailures} label="CI failures" color="var(--alert)" />
        <TipRow value={`${(sum(d.tokens) / 1e6).toFixed(1)}M`} label={`tokens · ${usd(sum(d.spendUSD))}`} color="var(--viz-1)" />
      </>,
    );
  };
  const leave = () => {
    setHover(null);
    tip.hide();
  };

  const stacked = (get: (d: ThroughputDay) => Record<string, number>, label: string, fmt: (n: number) => string) => {
    const y = scaleLinear()
      .domain([0, max(days, (d) => sum(get(d))) || 1])
      .nice(3)
      .range([H - M.bottom, M.top]);
    return (
      <Row label={label} y={y} fmt={fmt} width={width} hover={hover} x={x} onMove={onMove} onLeave={leave}>
        {days.map((d, i) => {
          let acc = 0;
          return (
            <g key={d.day}>
              {agents.map((a) => {
                const v = get(d)[a] ?? 0;
                if (!v) return null;
                const y0 = y(acc);
                acc += v;
                const y1 = y(acc);
                // 1px surface gap between segments.
                const hgt = Math.max(0, y0 - y1 - 1);
                return <rect key={a} x={x(i)} y={y1} width={x.bandwidth()} height={hgt} fill={color(a)} opacity={hover === null || hover === i ? 1 : 0.55} />;
              })}
            </g>
          );
        })}
      </Row>
    );
  };

  const latY = scaleLinear().domain([0, max(days, (d) => d.reviewLatencyHours ?? 0) || 1]).nice(3).range([H - M.bottom, M.top]);
  const latLine = line<ThroughputDay>()
    .defined((d) => d.reviewLatencyHours !== null)
    .x((_, i) => (x(i) ?? 0) + x.bandwidth() / 2)
    .y((d) => latY(d.reviewLatencyHours ?? 0));
  const ciY = scaleLinear().domain([0, Math.max(3, max(days, (d) => d.ciFailures) ?? 0)]).nice(3).range([H - M.bottom, M.top]);

  return (
    <div ref={box} className="flex w-full min-w-0 flex-col gap-1 overflow-hidden">
      <Legend>
        {groups.map((g, i) => (
          <LegendItem key={g} color={i % 2 ? "var(--viz-2)" : "var(--viz-1)"} label={`${g} agents`} shape="rect" />
        ))}
        <LegendItem color="var(--alert)" label="CI failures" shape="rect" />
      </Legend>
      <div ref={tip.ref} className="relative flex flex-col">
        {stacked((d) => d.prsMerged, "PRs merged", (n) => String(n))}
        <Row label="Review latency, h" y={latY} fmt={(n) => String(n)} width={width} hover={hover} x={x} onMove={onMove} onLeave={leave}>
          <path d={latLine(days) ?? ""} fill="none" stroke="var(--fg)" strokeWidth={1.25} />
          {days.map((d, i) =>
            d.reviewLatencyHours === null ? null : (
              <circle key={d.day} cx={(x(i) ?? 0) + x.bandwidth() / 2} cy={latY(d.reviewLatencyHours)} r={hover === i ? 3.5 : 1.75} fill="var(--surface)" stroke="var(--fg)" strokeWidth={1.25} />
            ),
          )}
        </Row>
        <Row label="CI failures" y={ciY} fmt={(n) => String(n)} width={width} hover={hover} x={x} onMove={onMove} onLeave={leave}>
          {days.map((d, i) => (d.ciFailures ? <rect key={d.day} x={x(i)} y={ciY(d.ciFailures)} width={x.bandwidth()} height={ciY(0) - ciY(d.ciFailures)} fill="var(--alert)" opacity={hover === null || hover === i ? 1 : 0.55} /> : null))}
        </Row>
        {stacked((d) => d.tokens, "Tokens", (n) => `${Math.round(n / 1e6)}M`)}
        <div className="flex justify-between font-mono text-[9px] text-muted" style={{ paddingLeft: M.left, paddingRight: M.right }}>
          <span>{days[0]?.day}</span>
          <span>{days[days.length - 1]?.day}</span>
        </div>
        {tip.node}
      </div>
    </div>
  );
}

function Row({
  label,
  y,
  fmt,
  width,
  hover,
  x,
  children,
  onMove,
  onLeave,
}: {
  label: string;
  y: ReturnType<typeof scaleLinear<number, number>>;
  fmt: (n: number) => string;
  width: number;
  hover: number | null;
  x: ReturnType<typeof scaleBand<number>>;
  children: ReactNode;
  onMove: (e: React.PointerEvent<SVGSVGElement>) => void;
  onLeave: () => void;
}) {
  const ticks = y.ticks(2);
  return (
    <div className="border-t border-border first:border-t-0">
      <span className="label">{label}</span>
      <svg viewBox={`0 0 ${width} ${H}`} width="100%" height={H} onPointerMove={onMove} onPointerLeave={onLeave} role="img" aria-label={label}>
        {ticks.map((t) => (
          <g key={t}>
            <line x1={M.left} x2={width - M.right} y1={y(t)} y2={y(t)} stroke="var(--grid)" strokeWidth={1} />
            <text x={M.left - 4} y={y(t) + 3} textAnchor="end" fontSize={8.5} className="fill-muted font-mono">
              {fmt(t)}
            </text>
          </g>
        ))}
        {children}
        {hover !== null && <line x1={(x(hover) ?? 0) + x.bandwidth() / 2} x2={(x(hover) ?? 0) + x.bandwidth() / 2} y1={0} y2={H} stroke="var(--fg)" strokeWidth={0.75} opacity={0.6} />}
      </svg>
    </div>
  );
}
