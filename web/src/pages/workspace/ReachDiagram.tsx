import type { WorkspaceDraft } from "@/api/types";
import { groupsInUse, reachability } from "@/lib/workspace";

const groupLabel = (g: string) => g || "(no group)";

/** Who can reach whom, group to group, as an SVG with a text equivalent. */
export function ReachDiagram({ draft }: { draft: WorkspaceDraft }) {
  const groups = groupsInUse(draft);
  const reach = reachability(draft);
  const W = 520;
  const H = groups.length > 1 ? 340 : 200;
  const cx = W / 2;
  const cy = H / 2;
  const R = groups.length > 1 ? Math.min(W, H) / 2 - 70 : 0;
  const pos = new Map(
    groups.map((g, i) => {
      const a = (2 * Math.PI * i) / groups.length - Math.PI / 2;
      return [g, { x: cx + R * Math.cos(a), y: cy + R * Math.sin(a) }];
    }),
  );
  const members = (g: string) => draft.rooms.filter((r) => r.group === g).map((r) => r.name);
  const nodeR = 34;
  return (
    <figure className="space-y-2">
      <svg viewBox={`0 0 ${W} ${H}`} className="h-auto w-full max-w-xl" role="img" aria-labelledby="reach-caption">
        <defs>
          <marker id="arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
            <path d="M0,0 L10,5 L0,10 z" fill="currentColor" />
          </marker>
        </defs>
        {reach
          .filter((e) => e.from !== e.to)
          .map((e, i) => {
            const a = pos.get(e.from);
            const b = pos.get(e.to);
            if (!a || !b) return null;
            const dx = b.x - a.x;
            const dy = b.y - a.y;
            const len = Math.hypot(dx, dy) || 1;
            // Offset both directions of a pair so they don't overlap.
            const ox = (-dy / len) * 8;
            const oy = (dx / len) * 8;
            const x1 = a.x + (dx / len) * nodeR + ox;
            const y1 = a.y + (dy / len) * nodeR + oy;
            const x2 = b.x - (dx / len) * (nodeR + 4) + ox;
            const y2 = b.y - (dy / len) * (nodeR + 4) + oy;
            return (
              <g key={i} className={e.mode === "open" ? "text-ok" : "text-warn"}>
                <line x1={x1} y1={y1} x2={x2} y2={y2} stroke="currentColor" strokeWidth={2} strokeDasharray={e.mode === "mediated" ? "6 4" : undefined} markerEnd="url(#arrow)" />
                <text x={(x1 + x2) / 2 + ox * 1.5} y={(y1 + y2) / 2 + oy * 1.5} fontSize={11} textAnchor="middle" fill="currentColor">
                  {e.mode}
                </text>
              </g>
            );
          })}
        {groups.map((g) => {
          const p = pos.get(g)!;
          const self = reach.some((e) => e.from === g && e.to === g);
          const m = members(g);
          return (
            <g key={g}>
              {self && (
                <path
                  d={`M ${p.x - 14} ${p.y - nodeR + 2} C ${p.x - 30} ${p.y - nodeR - 34}, ${p.x + 30} ${p.y - nodeR - 34}, ${p.x + 14} ${p.y - nodeR + 2}`}
                  fill="none"
                  stroke="currentColor"
                  className="text-ok"
                  strokeWidth={2}
                  markerEnd="url(#arrow)"
                />
              )}
              <circle cx={p.x} cy={p.y} r={nodeR} className="fill-surface-2 stroke-border" strokeWidth={2} />
              <text x={p.x} y={p.y + 4} textAnchor="middle" fontSize={12} fontWeight={600} className="fill-fg">
                {groupLabel(g)}
              </text>
              <text x={p.x} y={p.y + nodeR + 16} textAnchor="middle" fontSize={11} className="fill-muted">
                {m.length > 4 ? `${m.slice(0, 4).join(", ")} +${m.length - 4}` : m.join(", ") || "no agents"}
              </text>
            </g>
          );
        })}
      </svg>
      <figcaption id="reach-caption" className="text-sm">
        <ul className="list-disc pl-5">
          {reach.length === 0 && <li>No agent can message another.</li>}
          {reach.map((e, i) => (
            <li key={i}>
              {e.from === e.to ? `Agents in ${groupLabel(e.from)} reach each other` : `${groupLabel(e.from)} → ${groupLabel(e.to)}: ${e.label}`}
            </li>
          ))}
        </ul>
      </figcaption>
    </figure>
  );
}
