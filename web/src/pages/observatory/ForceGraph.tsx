import { forceCenter, forceCollide, forceLink, forceManyBody, forceSimulation, forceX, forceY, type Simulation, type SimulationLinkDatum, type SimulationNodeDatum } from "d3-force";
import { scaleSqrt } from "d3-scale";
import { useEffect, useMemo, useRef, useState } from "react";
import type { EdgeKind, Edges, HandoffKind } from "@/api/observatory";
import type { Presence } from "@/api/types";
import { presenceInfo } from "@/components/Presence";
import { Spinner } from "@/components/ui";
import { kindColor, Legend, LegendItem, presenceColor, TipRow, useTip, useWidth } from "./common";

interface Node extends SimulationNodeDatum {
  id: string;
  group: string;
  presence: Presence | "human";
  activity: number;
}

interface Link extends SimulationLinkDatum<Node> {
  kind: EdgeKind | HandoffKind;
  handoff: boolean;
  count: number;
}

const handoffLabel: Record<HandoffKind, string> = { reviewRequested: "review requested", assigned: "assigned", mentioned: "mentioned" };

/** Handoff graph (§8.7): messages solid, forge handoffs dashed; nodes sized by activity, colored by presence. */
export function ForceGraph({ edges }: { edges?: Edges }) {
  const [box, width] = useWidth<HTMLDivElement>(520);
  const height = Math.max(300, Math.min(460, width * 0.68));
  const tip = useTip();
  const [, setTick] = useState(0);
  const [hover, setHover] = useState<string | null>(null);
  const sim = useRef<Simulation<Node, Link> | null>(null);
  const nodesRef = useRef(new Map<string, Node>());
  const svg = useRef<SVGSVGElement>(null);

  const data = useMemo(() => {
    if (!edges) return null;
    const prev = nodesRef.current;
    const nodes: Node[] = [
      ...edges.agents.map((a) => ({ id: a.name, group: a.group, presence: a.presence as Node["presence"], activity: a.activity })),
      ...edges.humans.map((h) => ({ id: h, group: "people", presence: "human" as const, activity: edges.messages.filter((m) => m.from === h || m.to === h).reduce((s, m) => s + m.count, 0) })),
    ].map((n) => Object.assign(prev.get(n.id) ?? {}, n));
    nodesRef.current = new Map(nodes.map((n) => [n.id, n]));
    const pair = new Map<string, Link>();
    for (const m of edges.messages) {
      const [a, b] = [m.from, m.to].sort();
      const key = `${a}|${b}`;
      const l = pair.get(key);
      if (l) l.count += m.count;
      else pair.set(key, { source: a, target: b, kind: m.kind, handoff: false, count: m.count });
    }
    const links: Link[] = [...pair.values(), ...edges.handoffs.map((h) => ({ source: h.from, target: h.to, kind: h.kind, handoff: true, count: h.count }))];
    return { nodes, links };
  }, [edges]);

  const r = useMemo(() => scaleSqrt().domain([0, Math.max(1, ...(data?.nodes.map((n) => n.activity) ?? [1]))]).range([5, 17]), [data]);

  useEffect(() => {
    if (!data) return;
    const groups = [...new Set(data.nodes.map((n) => n.group))];
    const gx = (g: string) => (width * (groups.indexOf(g) + 1)) / (groups.length + 1);
    const s = forceSimulation<Node, Link>(data.nodes)
      .force("link", forceLink<Node, Link>(data.links).id((n) => n.id).distance((l) => (l.handoff ? 110 : 85)).strength((l) => (l.handoff ? 0.05 : Math.min(0.5, 0.04 + l.count / 400))))
      .force("charge", forceManyBody().strength(-320))
      .force("collide", forceCollide<Node>().radius((n) => r(n.activity) + 14))
      .force("x", forceX<Node>((n) => gx(n.group)).strength(0.12))
      .force("y", forceY(height / 2).strength(0.04))
      .force("center", forceCenter(width / 2, height / 2).strength(0.05))
      .alphaDecay(0.035)
      .on("tick", () => {
        for (const n of data.nodes) {
          const rad = r(n.activity) + 2;
          n.x = Math.max(rad, Math.min(width - rad, n.x ?? 0));
          n.y = Math.max(rad, Math.min(height - rad, n.y ?? 0));
        }
        setTick((t) => t + 1);
      });
    if (matchMedia("(prefers-reduced-motion: reduce)").matches) {
      s.stop();
      s.tick(300);
      setTick((t) => t + 1);
    }
    sim.current = s;
    return () => {
      s.stop();
    };
  }, [data, width, height, r]);

  if (!data) return <Spinner label="Loading" />;

  const point = (e: React.PointerEvent) => {
    const b = svg.current!.getBoundingClientRect();
    return { x: ((e.clientX - b.left) / b.width) * width, y: ((e.clientY - b.top) / b.height) * height };
  };
  const neighbors = (id: string) => new Set(data.links.flatMap((l) => {
    const s = (l.source as Node).id;
    const t = (l.target as Node).id;
    return s === id ? [t] : t === id ? [s] : [];
  }));
  const near = hover ? neighbors(hover) : null;
  const lit = (id: string) => !hover || id === hover || near!.has(id);

  return (
    <div ref={box} className="flex w-full min-w-0 flex-col gap-2 overflow-hidden">
      <Legend>
        <LegendItem color={kindColor.open} label="Messages" />
        <LegendItem color={kindColor.mediated} label="Crossing" />
        <LegendItem color="var(--fg)" label="Forge handoff" dashed />
        {(["working", "waitingOnHuman", "idle", "crashed", "offline"] as Presence[]).map((p) => (
          <LegendItem key={p} color={presenceColor[p]} label={presenceInfo[p].label} shape="dot" />
        ))}
      </Legend>
      <div ref={tip.ref} className="relative">
        <svg ref={svg} viewBox={`0 0 ${width} ${height}`} width="100%" height={height} role="img" aria-label="Handoff graph" className="touch-none select-none">
          <g>
            {data.links.map((l, i) => {
              const s = l.source as Node;
              const t = l.target as Node;
              const on = lit(s.id) && lit(t.id) && (!hover || s.id === hover || t.id === hover);
              return (
                <line
                  key={i}
                  x1={s.x}
                  y1={s.y}
                  x2={t.x}
                  y2={t.y}
                  stroke={l.handoff ? "var(--fg)" : kindColor[l.kind as EdgeKind]}
                  strokeWidth={l.handoff ? 1 : Math.min(4, 0.6 + l.count / 30)}
                  strokeDasharray={l.handoff ? "3 3" : undefined}
                  opacity={on ? (l.handoff ? 0.55 : 0.75) : 0.08}
                  className="transition-opacity duration-300"
                />
              );
            })}
          </g>
          {data.nodes.map((n) => {
            const rad = r(n.activity);
            const human = n.presence === "human";
            const label = human ? "person" : presenceInfo[n.presence as Presence].label;
            return (
              <g
                key={n.id}
                transform={`translate(${n.x ?? 0},${n.y ?? 0})`}
                opacity={lit(n.id) ? 1 : 0.25}
                className="cursor-grab transition-opacity duration-300"
                tabIndex={0}
                role="button"
                aria-label={`${n.id}: ${label}, ${n.activity} events`}
                onPointerEnter={() => setHover(n.id)}
                onPointerMove={(e) => tip.show(e, <><TipRow value={n.activity} label={`events, ${n.id}`} color={human ? "var(--viz-2)" : presenceColor[n.presence as Presence]} /><div className="label">{label} · {n.group}</div></>)}
                onPointerLeave={() => {
                  setHover(null);
                  tip.hide();
                }}
                onFocus={(e) => {
                  setHover(n.id);
                  tip.showAt(e.currentTarget, <TipRow value={n.activity} label={`events, ${n.id}`} />);
                }}
                onBlur={() => {
                  setHover(null);
                  tip.hide();
                }}
                onPointerDown={(e) => {
                  (e.currentTarget as Element).setPointerCapture(e.pointerId);
                  const p = point(e);
                  n.fx = p.x;
                  n.fy = p.y;
                  sim.current?.alphaTarget(0.3).restart();
                }}
                onPointerUp={(e) => {
                  (e.currentTarget as Element).releasePointerCapture(e.pointerId);
                  n.fx = null;
                  n.fy = null;
                  sim.current?.alphaTarget(0);
                }}
                onPointerMoveCapture={(e) => {
                  if (n.fx == null) return;
                  const p = point(e);
                  n.fx = p.x;
                  n.fy = p.y;
                }}
              >
                <circle r={rad + 8} fill="transparent" />
                <circle
                  r={rad}
                  fill={human ? "var(--surface)" : presenceColor[n.presence as Presence]}
                  stroke={human ? "var(--viz-2)" : "var(--surface)"}
                  strokeWidth={human ? 2 : 2}
                />
                {n.presence === "waitingOnHuman" && <circle r={rad + 4} fill="none" stroke="var(--alert)" strokeWidth={1} className="live-dot" />}
                <text y={rad + 11} textAnchor="middle" fontSize={10} className="fill-fg font-mono" paintOrder="stroke" stroke="var(--surface)" strokeWidth={3}>
                  {n.id}
                </text>
              </g>
            );
          })}
        </svg>
        {tip.node}
      </div>
      <p className="sr-only">
        {data.links
          .filter((l) => l.handoff)
          .map((l) => `${(l.source as Node).id} ${handoffLabel[l.kind as HandoffKind]} ${(l.target as Node).id} ${l.count} times`)
          .join("; ")}
      </p>
    </div>
  );
}
