import { useQuery } from "@tanstack/react-query";
import { descending } from "d3-array";
import { chord, ribbon, type Chord as ChordT } from "d3-chord";
import { arc } from "d3-shape";
import { useMemo, useState } from "react";
import { api } from "@/api";
import type { EdgeKind, Edges, ObsWindow } from "@/api/observatory";
import { Spinner } from "@/components/ui";
import { time } from "@/lib/format";
import { kindColor, kindLabel, Legend, LegendItem, TipRow, useTip, useWidth } from "./common";

export interface ChordModel {
  parties: { id: string; group: string }[];
  matrix: number[][];
  /** Kind of the pair i<j, keyed "i,j". */
  pairKind: Map<string, EdgeKind>;
}

/** Orders parties by group (people last) and builds the message matrix. */
export function chordModel(e: Edges): ChordModel {
  const parties = [
    ...[...e.agents].sort((a, b) => a.group.localeCompare(b.group) || a.name.localeCompare(b.name)).map((a) => ({ id: a.name, group: a.group })),
    ...e.humans.map((h) => ({ id: h, group: "people" })),
  ];
  const idx = new Map(parties.map((p, i) => [p.id, i]));
  const matrix = parties.map(() => parties.map(() => 0));
  const pairKind = new Map<string, EdgeKind>();
  const rank: Record<EdgeKind, number> = { open: 0, human: 1, mediated: 2 };
  for (const m of e.messages) {
    const i = idx.get(m.from);
    const j = idx.get(m.to);
    if (i === undefined || j === undefined) continue;
    matrix[i][j] += m.count;
    const key = `${Math.min(i, j)},${Math.max(i, j)}`;
    const prev = pairKind.get(key);
    if (!prev || rank[m.kind] > rank[prev]) pairKind.set(key, m.kind);
  }
  return { parties, matrix, pairKind };
}

/** Who talks to whom (§8.7): a chord diagram of message volume, ribbons colored by kind. */
export function ChordChart({ workspace, window, edges }: { workspace: string; window: ObsWindow; edges?: Edges }) {
  const [box, width] = useWidth<HTMLDivElement>(480);
  const tip = useTip();
  const [hover, setHover] = useState<{ type: "arc"; i: number } | { type: "pair"; i: number; j: number } | null>(null);
  const [selected, setSelected] = useState<[string, string] | null>(null);
  const model = useMemo(() => edges && chordModel(edges), [edges]);
  const size = Math.min(width, 520);
  // Room outside the ring for names (up to ~11 characters) and the group band.
  const outer = size / 2 - 96;
  const inner = outer - 7;

  const layout = useMemo(() => {
    if (!model) return null;
    const c = chord().padAngle(0.035).sortSubgroups(descending)(model.matrix);
    return c;
  }, [model]);

  if (!model || !layout) return <Spinner label="Loading" />;
  const arcGen = arc<{ startAngle: number; endAngle: number }>().innerRadius(inner).outerRadius(outer);
  const ribbonGen = ribbon<ChordT, ChordT["source"]>().radius(inner - 2);
  const groupBand = arc<{ startAngle: number; endAngle: number }>().innerRadius(outer + 76).outerRadius(outer + 77);
  const kindOf = (c: ChordT): EdgeKind => model.pairKind.get(`${Math.min(c.source.index, c.target.index)},${Math.max(c.source.index, c.target.index)}`) ?? "open";
  const active = (c: ChordT) =>
    !hover ||
    (hover.type === "arc" && (c.source.index === hover.i || c.target.index === hover.i)) ||
    (hover.type === "pair" && ((c.source.index === hover.i && c.target.index === hover.j) || (c.source.index === hover.j && c.target.index === hover.i)));

  // Bands per group, spanning that group's arcs.
  const bands: { group: string; start: number; end: number }[] = [];
  for (const g of layout.groups) {
    const group = model.parties[g.index].group;
    const last = bands[bands.length - 1];
    if (last && last.group === group) last.end = g.endAngle;
    else bands.push({ group, start: g.startAngle, end: g.endAngle });
  }
  const total = (i: number) => model.matrix[i].reduce((a, b) => a + b, 0) + model.matrix.reduce((a, row) => a + row[i], 0);

  return (
    <div ref={box} className="flex w-full min-w-0 flex-col gap-2 overflow-hidden">
      <Legend>
        {(["open", "mediated", "human"] as EdgeKind[]).map((k) => (
          <LegendItem key={k} color={kindColor[k]} label={kindLabel[k]} shape="rect" />
        ))}
      </Legend>
      <div ref={tip.ref} className="relative mx-auto" style={{ width: size }}>
        <svg viewBox={`${-size / 2} ${-size / 2} ${size} ${size}`} width={size} height={size} role="img" aria-label="Messages between agents and people" onPointerLeave={() => { setHover(null); tip.hide(); }}>
          {bands.map((b) => {
            const mid = (b.start + b.end) / 2;
            const r = outer + 84;
            const deg = (mid * 180) / Math.PI;
            const bottom = deg > 90 && deg < 270;
            return (
              <g key={b.group}>
                <path d={groupBand({ startAngle: b.start, endAngle: b.end })!} fill="var(--fg)" opacity={0.5} />
                {/* Tangential, so the label takes little radial room. */}
                <text
                  transform={`rotate(${deg}) translate(0,${-r}) ${bottom ? "rotate(180)" : ""}`}
                  textAnchor="middle"
                  dominantBaseline="middle"
                  className="fill-muted font-mono"
                  fontSize={9}
                  letterSpacing="0.1em"
                >
                  {b.group.toUpperCase()}
                </text>
              </g>
            );
          })}
          <g fillOpacity={0.62}>
            {layout.map((c) => {
              const a = model.parties[c.source.index].id;
              const b = model.parties[c.target.index].id;
              const k = kindOf(c);
              const vol = c.source.value + c.target.value;
              return (
                <path
                  key={`${c.source.index}-${c.target.index}`}
                  d={ribbonGen(c) as unknown as string}
                  fill={kindColor[k]}
                  stroke="var(--surface)"
                  strokeWidth={0.75}
                  opacity={active(c) ? 1 : 0.12}
                  className="cursor-pointer transition-opacity duration-300"
                  tabIndex={0}
                  role="button"
                  aria-label={`${a} and ${b}: ${vol} messages, ${kindLabel[k]}`}
                  onPointerMove={(e) => {
                    setHover({ type: "pair", i: c.source.index, j: c.target.index });
                    tip.show(e, <PairTip a={a} b={b} ab={model.matrix[c.source.index][c.target.index]} ba={model.matrix[c.target.index][c.source.index]} kind={k} />);
                  }}
                  onFocus={(e) => tip.showAt(e.currentTarget, <PairTip a={a} b={b} ab={model.matrix[c.source.index][c.target.index]} ba={model.matrix[c.target.index][c.source.index]} kind={k} />)}
                  onBlur={tip.hide}
                  onClick={() => setSelected([a, b])}
                  onKeyDown={(e) => (e.key === "Enter" || e.key === " ") && setSelected([a, b])}
                />
              );
            })}
          </g>
          {layout.groups.map((g) => {
            const p = model.parties[g.index];
            const mid = (g.startAngle + g.endAngle) / 2;
            const flip = mid > Math.PI;
            return (
              <g
                key={p.id}
                onPointerMove={(e) => {
                  setHover({ type: "arc", i: g.index });
                  tip.show(e, <TipRow value={total(g.index)} label={`messages, ${p.id}`} />);
                }}
              >
                <path d={arcGen(g)!} fill={p.group === "people" ? "var(--viz-2)" : "var(--fg)"} opacity={hover && hover.type === "arc" && hover.i !== g.index ? 0.3 : 0.85} />
                <text
                  transform={`rotate(${(mid * 180) / Math.PI - 90}) translate(${outer + 6}) ${flip ? "rotate(180)" : ""}`}
                  textAnchor={flip ? "end" : "start"}
                  dominantBaseline="middle"
                  fontSize={10.5}
                  className="fill-fg font-mono"
                >
                  {p.id}
                </text>
              </g>
            );
          })}
        </svg>
        {tip.node}
      </div>
      {selected && <PairMessages workspace={workspace} window={window} a={selected[0]} b={selected[1]} onClose={() => setSelected(null)} />}
    </div>
  );
}

function PairTip({ a, b, ab, ba, kind }: { a: string; b: string; ab: number; ba: number; kind: EdgeKind }) {
  return (
    <>
      <TipRow value={ab} label={`${a} → ${b}`} color={kindColor[kind]} />
      <TipRow value={ba} label={`${b} → ${a}`} color={kindColor[kind]} />
      <div className="label mt-1">{kindLabel[kind]} · click for messages</div>
    </>
  );
}

function PairMessages({ workspace, window, a, b, onClose }: { workspace: string; window: ObsWindow; a: string; b: string; onClose: () => void }) {
  const q = useQuery({ queryKey: ["obs", "pair", workspace, window, a, b], queryFn: () => api().observatory.pairMessages(workspace, a, b, window) });
  return (
    <div className="border-t border-border pt-2" data-testid="pair-messages">
      <div className="flex items-baseline justify-between">
        <span className="label !text-fg">
          {a} ⇄ {b} · {q.data?.length ?? "…"} messages
        </span>
        <button type="button" className="label min-h-8 px-2 hover:!text-fg" onClick={onClose}>
          Close
        </button>
      </div>
      <p className="text-xs text-muted">Metadata only; bodies are in the audit log for roles that may read them.</p>
      <ul className="mt-1 max-h-44 overflow-auto font-mono text-[11px]">
        {q.data?.map((m) => (
          <li key={m.id} className="flex gap-3 border-b border-border/60 py-0.5">
            <span className="text-muted tabular-nums">{time(m.at)}</span>
            <span>
              {m.from} → {m.to}
            </span>
            {m.bytes !== undefined && <span className="ml-auto text-muted tabular-nums">{m.bytes} B</span>}
          </li>
        ))}
      </ul>
    </div>
  );
}
