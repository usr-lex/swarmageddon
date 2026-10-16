import type { AgentAxes } from "@/api/observatory";
import { Spinner } from "@/components/ui";
import { TipRow, useTip } from "./common";

type Axis = "reviews" | "issues" | "pullRequests" | "commits";

/** GitHub's order: code review up, issues right, pull requests down, commits left. */
const axes: { key: Axis; label: string; dx: number; dy: number }[] = [
  { key: "reviews", label: "Review", dx: 0, dy: -1 },
  { key: "issues", label: "Issues", dx: 1, dy: 0 },
  { key: "pullRequests", label: "PRs", dx: 0, dy: 1 },
  { key: "commits", label: "Commits", dx: -1, dy: 0 },
];

/** Shares of the four axes, in percent. */
export function shares(a: AgentAxes): Record<Axis, number> {
  const total = a.commits + a.pullRequests + a.reviews + a.issues;
  const p = (n: number) => (total ? Math.round((n / total) * 100) : 0);
  return { reviews: p(a.reviews), issues: p(a.issues), pullRequests: p(a.pullRequests), commits: p(a.commits) };
}

/** What the agent leans to: building (commits+PRs), reviewing, or coordinating (issues+messages). */
export function leaning(a: AgentAxes): "building" | "reviewing" | "coordinating" | "quiet" {
  const build = a.commits + a.pullRequests * 3;
  const review = a.reviews * 2;
  const coord = a.issues * 2 + a.messages * 0.5;
  const max = Math.max(build, review, coord);
  if (max === 0) return "quiet";
  return max === build ? "building" : max === review ? "reviewing" : "coordinating";
}

const expected: Record<string, ReturnType<typeof leaning>> = {
  implementer: "building",
  reviewer: "reviewing",
  coordinator: "coordinating",
  chronicler: "coordinating",
  qa: "building",
};

function Mini({ a, maxMessages, tip }: { a: AgentAxes; maxMessages: number; tip: ReturnType<typeof useTip> }) {
  // Wider than tall, so the side labels fit beside the arms.
  const W = 200;
  const S = 136;
  const c = S / 2;
  const cx = W / 2;
  const R = 46;
  const sh = shares(a);
  // Polygon distance: share mapped onto the arm, with a small floor so zero stays visible.
  const pt = (ax: (typeof axes)[number]) => {
    const d = 3 + (sh[ax.key] / 100) * (R - 3);
    return [cx + ax.dx * d, c + ax.dy * d];
  };
  const poly = axes.map((ax) => pt(ax).join(",")).join(" ");
  const ring = maxMessages ? 6 + (a.messages / maxMessages) * (R - 6) : 0;
  const lean = leaning(a);
  const mismatch = expected[a.role] && lean !== "quiet" && expected[a.role] !== lean;
  return (
    <figure className="flex min-w-0 flex-col items-center" data-testid="axes-agent">
      <svg
        viewBox={`0 0 ${W} ${S}`}
        width="100%"
        style={{ maxWidth: W * 1.3 }}
        role="img"
        aria-label={`${a.name}: commits ${sh.commits}%, pull requests ${sh.pullRequests}%, code review ${sh.reviews}%, issues ${sh.issues}%, ${a.messages} messages`}
        onPointerMove={(e) =>
          tip.show(
            e,
            <>
              <TipRow value={a.commits} label="commits" />
              <TipRow value={a.pullRequests} label="pull requests" />
              <TipRow value={a.reviews} label="reviews" />
              <TipRow value={a.issues} label="issues" />
              <TipRow value={a.messages} label="messages" color="var(--viz-2)" />
            </>,
          )
        }
        onPointerLeave={tip.hide}
      >
        <circle cx={cx} cy={c} r={ring} fill="none" stroke="var(--viz-2)" strokeWidth={1} strokeDasharray="1.5 2.5" />
        {axes.map((ax) => (
          <g key={ax.key}>
            <line x1={cx} y1={c} x2={cx + ax.dx * R} y2={c + ax.dy * R} stroke="var(--border)" strokeWidth={1} />
            <text
              x={cx + ax.dx * (R + 5)}
              y={c + ax.dy * (R + 6) + (ax.dy === 0 ? 3 : ax.dy > 0 ? 8 : -2)}
              textAnchor={ax.dx > 0 ? "start" : ax.dx < 0 ? "end" : "middle"}
              fontSize={8}
              className="fill-muted font-mono uppercase"
            >
              {sh[ax.key]}% {ax.label}
            </text>
          </g>
        ))}
        <polygon points={poly} fill="color-mix(in srgb, var(--viz-1) 18%, transparent)" stroke="var(--viz-1)" strokeWidth={1.5} strokeLinejoin="round" />
        {axes.map((ax) => {
          const [x, y] = pt(ax);
          return <circle key={ax.key} cx={x} cy={y} r={2} fill="var(--surface)" stroke="var(--viz-1)" strokeWidth={1.5} />;
        })}
      </svg>
      <figcaption className="text-center">
        <span className="font-mono text-[11px]">{a.name}</span>
        <span className="label block">
          {a.role} · <span className={mismatch ? "!text-alert" : ""}>{lean}</span>
        </span>
      </figcaption>
    </figure>
  );
}

/** Activity axes (§8.7): per agent, the share of commits, PRs, review and issues, and a messages ring. */
export function AxesPanel({ data }: { data?: AgentAxes[] }) {
  const tip = useTip();
  if (!data) return <Spinner label="Loading" />;
  const maxMessages = Math.max(0, ...data.map((a) => a.messages));
  return (
    <div ref={tip.ref} className="relative">
      <div className="grid grid-cols-2 gap-2 sm:grid-cols-3 lg:grid-cols-5">
        {data.map((a) => (
          <Mini key={a.name} a={a} maxMessages={maxMessages} tip={tip} />
        ))}
      </div>
      <p className="label mt-2">
        Polygon: share of commits, PRs, code review and issues · dotted ring: messages, relative to the busiest agent · <span className="!text-alert">orange</span>: leaning differs from role
      </p>
      {tip.node}
    </div>
  );
}
