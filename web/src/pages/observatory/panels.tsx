import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { api } from "@/api";
import { consume } from "@/api/stream";
import type { ActivityEvent, ActivityKind, NowRow } from "@/api/observatory";
import type { Presence } from "@/api/types";
import { PresenceBadge } from "@/components/Presence";
import { Button, ErrorText, Spinner, cx } from "@/components/ui";
import { ago } from "@/lib/format";
import { renderMarkdown } from "@/lib/markdown";
import { since } from "./common";

/** Swarm status (§8.7): the chronicler's latest report and "request an update now". */
export function StatusReport({ workspace }: { workspace: string }) {
  const qc = useQueryClient();
  const [waitingSince, setWaitingSince] = useState<string | null>(null);
  const latest = useQuery({
    queryKey: ["obs", "report", workspace],
    queryFn: () => api().observatory.latestReport(workspace),
    refetchInterval: waitingSince ? 1000 : 60_000,
  });
  const [history, setHistory] = useState(false);
  const older = useQuery({ queryKey: ["obs", "reports", workspace], queryFn: () => api().observatory.reports(workspace, 20), enabled: history });
  const request = useMutation({
    mutationFn: () => api().observatory.requestReport(workspace),
    onSuccess: () => setWaitingSince(latest.data?.at ?? ""),
  });
  useEffect(() => {
    if (waitingSince !== null && latest.data && latest.data.at !== waitingSince) {
      setWaitingSince(null);
      void qc.invalidateQueries({ queryKey: ["obs", "reports", workspace] });
    }
  }, [latest.data, waitingSince, qc, workspace]);
  const r = latest.data;
  return (
    <div className="flex h-full flex-col gap-2">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <span className="label" data-testid="report-time">
          {r ? `${ago(r.at)} · ${r.author}` : "No report yet"}
        </span>
        <Button onClick={() => request.mutate()} busy={request.isPending || waitingSince !== null} className="!min-h-8 font-mono text-[11px] uppercase tracking-wider">
          {waitingSince !== null ? "Waiting for the chronicler" : "Request an update now"}
        </Button>
      </div>
      <ErrorText error={request.error ?? latest.error} />
      {latest.isPending && <Spinner label="Loading" />}
      {r && (
        <article key={r.id} className="report flash flex-1 overflow-auto text-sm leading-relaxed" data-testid="report">
          {renderMarkdown(r.markdown)}
        </article>
      )}
      {!r && !latest.isPending && (
        <p className="text-sm text-muted">
          No chronicler has reported yet. Add a room with the <code>chronicler</code> role to get a report every few hours.
        </p>
      )}
      <button type="button" className="label self-start hover:!text-fg" aria-expanded={history} onClick={() => setHistory(!history)}>
        {history ? "Hide earlier reports" : "Earlier reports"}
      </button>
      {history && (
        <ol className="max-h-64 space-y-3 overflow-auto border-t border-border pt-2">
          {older.data?.slice(1).map((x) => (
            <li key={x.id} className="report text-xs">
              <span className="label">{ago(x.at)}</span>
              {renderMarkdown(x.markdown)}
            </li>
          ))}
        </ol>
      )}
    </div>
  );
}

const nowOrder: Record<Presence, number> = { waitingOnHuman: 0, crashed: 1, working: 2, idle: 3, offline: 4 };

/** Now (§8.7): one row per agent, waiting on a human first. */
export function NowTable({ workspace, rows }: { workspace: string; rows?: NowRow[] }) {
  const [, setTick] = useState(0);
  useEffect(() => {
    const t = setInterval(() => setTick((x) => x + 1), 30_000);
    return () => clearInterval(t);
  }, []);
  if (!rows) return <Spinner label="Loading" />;
  const sorted = [...rows].sort((a, b) => nowOrder[a.presence] - nowOrder[b.presence] || a.name.localeCompare(b.name));
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left text-[13px]">
        <caption className="sr-only">Agents now</caption>
        <thead>
          <tr className="border-b border-border">
            {["Agent", "State", "For", "Current task", "Last touched"].map((h) => (
              <th key={h} scope="col" className="label py-1 pr-3 font-normal">
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {sorted.map((r) => (
            <tr key={r.name} data-testid="now-row" data-agent={r.name} className={cx("border-b border-border/60", r.presence === "waitingOnHuman" && "bg-alert/5")}>
              <td className="py-1.5 pr-3 font-mono whitespace-nowrap">
                <Link to="/rooms/$workspace/$room" params={{ workspace, room: r.name }} search={{ tab: "agent" }} className="hover:underline">
                  {r.name}
                </Link>
                <span className="label ml-2">{r.role}</span>
              </td>
              <td className="py-1.5 pr-3">
                <PresenceBadge presence={r.presence} />
              </td>
              <td className="py-1.5 pr-3 font-mono text-xs whitespace-nowrap text-muted tabular-nums">{since(r.since)}</td>
              <td className="max-w-72 truncate py-1.5 pr-3">{r.currentTask || <span className="text-muted">—</span>}</td>
              <td className="py-1.5 pr-3 text-xs whitespace-nowrap">
                {r.lastTouched ? (
                  <span title={r.lastTouched.title}>
                    <span className="font-mono">
                      {r.lastTouched.repo}
                      {r.lastTouched.kind === "pr" ? "!" : "#"}
                      {r.lastTouched.number}
                    </span>{" "}
                    <span className="text-muted">{r.lastTouched.title}</span>
                  </span>
                ) : (
                  <span className="text-muted">—</span>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export const kindGroups: { id: string; label: string; kinds: ActivityKind[] }[] = [
  { id: "code", label: "Code", kinds: ["commit", "prOpened", "prMerged", "prClosed"] },
  { id: "review", label: "Review", kinds: ["review", "comment"] },
  { id: "issues", label: "Issues", kinds: ["issueOpened", "issueClosed"] },
  { id: "messages", label: "Messages", kinds: ["message", "crossing", "refusal"] },
  { id: "ci", label: "CI", kinds: ["ciFailed", "ciPassed"] },
];

const kindGlyph: Partial<Record<ActivityKind, string>> = {
  commit: "●",
  prOpened: "◇",
  prMerged: "◆",
  prClosed: "◌",
  review: "✓",
  comment: "¶",
  issueOpened: "○",
  issueClosed: "⊘",
  message: "→",
  crossing: "⇢",
  refusal: "⨯",
  ciFailed: "✕",
  ciPassed: "✓",
  presence: "·",
};

/** Timeline (§8.7): a merged, filterable, live feed. */
export function Timeline({ workspace, agents }: { workspace: string; agents: string[] }) {
  const [groups, setGroups] = useState<string[]>([]);
  const [actor, setActor] = useState("");
  const [fresh, setFresh] = useState<Set<string>>(new Set());
  const kinds = kindGroups.filter((g) => groups.includes(g.id)).flatMap((g) => g.kinds);
  const qc = useQueryClient();
  const key = ["obs", "timeline", workspace, kinds.join(","), actor];
  const q = useQuery({ queryKey: key, queryFn: () => api().observatory.timeline(workspace, { kinds, actor, limit: 150 }) });
  useEffect(() => {
    const ctl = new AbortController();
    void consume(
      (s) => api().observatory.watchTimeline(workspace, s),
      (e: ActivityEvent) => {
        if ((kinds.length && !kinds.includes(e.kind)) || (actor && e.actor !== actor)) return;
        qc.setQueryData<ActivityEvent[]>(key, (old) => (old && !old.some((x) => x.id === e.id) ? [e, ...old].slice(0, 300) : old));
        setFresh((f) => new Set(f).add(e.id));
      },
      ctl.signal,
    );
    return () => ctl.abort();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [workspace, kinds.join(","), actor, qc]);
  const toggle = (id: string) => setGroups((g) => (g.includes(id) ? g.filter((x) => x !== id) : [...g, id]));
  return (
    <div className="flex h-full flex-col gap-2">
      <div className="flex flex-wrap items-center gap-1">
        {kindGroups.map((g) => (
          <button
            key={g.id}
            type="button"
            aria-pressed={groups.includes(g.id)}
            onClick={() => toggle(g.id)}
            className={cx("min-h-8 border px-2 font-mono text-[10.5px] tracking-wider uppercase", groups.includes(g.id) ? "border-fg bg-fg text-bg" : "border-border text-muted hover:text-fg")}
          >
            {g.label}
          </button>
        ))}
        <select aria-label="Agent" value={actor} onChange={(e) => setActor(e.target.value)} className="ml-auto min-h-8 border border-border bg-surface px-1 font-mono text-[11px]">
          <option value="">all agents</option>
          {agents.map((a) => (
            <option key={a}>{a}</option>
          ))}
        </select>
      </div>
      {q.isPending && <Spinner label="Loading" />}
      <ol className="max-h-[28rem] flex-1 overflow-auto font-mono text-[11.5px]" aria-label="Timeline" aria-live="polite" data-testid="timeline">
        {q.data?.map((e) => (
          <li key={e.id} className={cx("grid grid-cols-[2.6rem_1rem_6.2rem_1fr] items-baseline gap-1 border-b border-border/50 py-1", fresh.has(e.id) && "flash")}>
            <time dateTime={e.at} className="text-muted tabular-nums">
              {new Date(e.at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false })}
            </time>
            <span aria-hidden className={cx(e.kind === "ciFailed" || e.kind === "refusal" ? "text-alert" : e.kind === "crossing" ? "text-viz-1" : "text-muted")}>
              {kindGlyph[e.kind] ?? "·"}
            </span>
            <span className="truncate">{e.actor}</span>
            <span className="min-w-0 truncate font-sans text-[12.5px]">
              {e.summary}
              {e.repo && <span className="ml-1 font-mono text-[10.5px] text-muted">{e.repo}</span>}
            </span>
          </li>
        ))}
      </ol>
    </div>
  );
}
