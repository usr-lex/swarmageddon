import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";
import { api } from "@/api";
import { consume } from "@/api/stream";
import type { EngineItem, LogComponent, PruneKind, RoomDetail, RoomSummary, TranscriptEntry } from "@/api/types";
import { PresenceBadge } from "@/components/Presence";
import { TerminalView } from "@/components/TerminalView";
import { WardenPanel } from "@/components/Warden";
import { Badge, Button, Card, Checkbox, Empty, ErrorText, Meter, Select, Spinner, cx } from "@/components/ui";
import { ago, bytes, percent } from "@/lib/format";
import { useApprovals } from "@/lib/live";

export function AgentTab({ workspace, room, canOperate }: { workspace: string; room: string; canOperate: boolean }) {
  const [view, setView] = useState<"transcript" | "terminal">(canOperate ? "terminal" : "transcript");
  const [control, setControl] = useState(false);
  return (
    <div className="space-y-3">
      {canOperate && (
        <div className="flex flex-wrap items-center gap-2">
          <div role="group" aria-label="Agent view" className="flex rounded-xs border border-border">
            {(["terminal", "transcript"] as const).map((v) => (
              <button
                key={v}
                type="button"
                aria-pressed={view === v}
                onClick={() => setView(v)}
                className={cx("min-h-10 px-3 text-sm capitalize", view === v ? "bg-surface-2 font-medium" : "text-muted")}
              >
                {v}
              </button>
            ))}
          </div>
          {view === "terminal" && (
            <Checkbox
              label={<span className={control ? "font-medium text-warn" : ""}>Take control (keystrokes go to the agent's session)</span>}
              checked={control}
              onChange={(e) => setControl(e.target.checked)}
            />
          )}
        </div>
      )}
      {view === "terminal" && canOperate ? (
        <TerminalView workspace={workspace} room={room} mode="agent" readOnly={!control} />
      ) : (
        <Transcript workspace={workspace} room={room} />
      )}
    </div>
  );
}

const kindStyle: Record<TranscriptEntry["kind"], string> = {
  user: "border-info/40 bg-info/5",
  assistant: "border-border bg-surface",
  tool: "border-warn/40 bg-warn/5 font-mono text-xs",
  toolResult: "border-border bg-surface-2 font-mono text-xs",
  system: "border-transparent text-muted text-xs",
};

function Transcript({ workspace, room }: { workspace: string; room: string }) {
  const [entries, setEntries] = useState<TranscriptEntry[]>([]);
  const [error, setError] = useState<unknown>(null);
  const end = useRef<HTMLDivElement>(null);
  useEffect(() => {
    setEntries([]);
    setError(null);
    const ctl = new AbortController();
    void consume(
      (s) => api().watchTranscript(workspace, room, s),
      (e) => setEntries((old) => (old.some((x) => x.id === e.id) ? old : [...old.slice(-999), e])),
      ctl.signal,
      setError,
    );
    return () => ctl.abort();
  }, [workspace, room]);
  useEffect(() => end.current?.scrollIntoView?.({ block: "end" }), [entries.length]);
  return (
    <div className="h-[60vh] min-h-64 overflow-y-auto rounded-xs border border-border p-2" aria-label="Transcript" aria-live="polite" role="log">
      {entries.length === 0 && (error ? <ErrorText error={error} /> : <Spinner label="Waiting for the transcript" />)}
      <ol className="space-y-2">
        {entries.map((e) => (
          <li key={e.id} className={cx("rounded-xs border p-2 text-sm whitespace-pre-wrap break-words", kindStyle[e.kind])}>
            <span className="sr-only">{e.kind}: </span>
            {e.text}
          </li>
        ))}
      </ol>
      <div ref={end} />
    </div>
  );
}

export function ActivityTab({ detail, live, canOperate }: { detail: RoomDetail; live: RoomSummary; canOperate: boolean }) {
  const approvals = useApprovals();
  const mine = approvals.data?.filter((a) => a.workspace === live.workspace && a.room === live.name) ?? [];
  return (
    <div className="grid gap-4 lg:grid-cols-2">
      <Card title="Now">
        <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-sm">
          <dt className="text-muted">Presence</dt>
          <dd>
            <PresenceBadge presence={live.presence} />
          </dd>
          <dt className="text-muted">Task</dt>
          <dd>{live.currentTask || "—"}</dd>
          <dt className="text-muted">Tokens</dt>
          <dd>
            {detail.inputTokens.toLocaleString()} in, {detail.outputTokens.toLocaleString()} out, context {detail.contextTokens.toLocaleString()}
          </dd>
          <dt className="text-muted">Forgejo user</dt>
          <dd>{detail.forgeUser}</dd>
          <dt className="text-muted">Repos</dt>
          <dd>{detail.repos.join(", ") || "—"}</dd>
          <dt className="text-muted">Image</dt>
          <dd>{detail.imageVersion}</dd>
        </dl>
        <ul className="mt-3 flex flex-wrap gap-1" aria-label="Conditions">
          {detail.conditions.map((c) => (
            <li key={c.type}>
              <Badge tone={c.status === "True" ? "ok" : c.status === "False" ? "bad" : "muted"}>
                {c.type}
                {c.message && `: ${c.message}`}
              </Badge>
            </li>
          ))}
        </ul>
      </Card>
      <Card title="Warden" className="lg:col-span-2">
        <WardenPanel workspace={live.workspace} room={live.name} canOperate={canOperate} />
      </Card>
      <Card title="Pending approvals">
        {mine.length === 0 ? (
          <p className="text-sm text-muted">None.</p>
        ) : (
          <ul className="space-y-1 text-sm">
            {mine.map((a) => (
              <li key={a.id}>
                <Link to="/approvals/$id" params={{ id: a.id }} className="text-accent underline">
                  {a.tool}: {a.summary}
                </Link>
              </li>
            ))}
          </ul>
        )}
      </Card>
      <Card title="Presence history">
        <ol className="space-y-1 text-sm">
          {[...detail.presenceHistory].reverse().map((p, i) => (
            <li key={i} className="flex items-center gap-2">
              <PresenceBadge presence={p.presence} /> <span className="text-muted">{ago(p.at)}</span>
            </li>
          ))}
        </ol>
      </Card>
      <Card title="Recent messages" actions={<Link to="/inbox/$address" params={{ address: `${live.name}@${live.workspace}` }} className="text-sm text-accent underline">Open conversation</Link>}>
        {detail.recentMessages.length === 0 ? (
          <p className="text-sm text-muted">None.</p>
        ) : (
          <ul className="space-y-2 text-sm">
            {detail.recentMessages.map((m) => (
              <li key={m.id}>
                <p className="text-xs text-muted">
                  {m.from} → {m.to} · {ago(m.at)}
                </p>
                <p className="whitespace-pre-wrap">{m.body}</p>
              </li>
            ))}
          </ul>
        )}
      </Card>
    </div>
  );
}

export function ResourcesTab({ workspace, room }: { workspace: string; room: string }) {
  const q = useQuery({ queryKey: ["resources", workspace, room], queryFn: () => api().getResources(workspace, room), refetchInterval: 5000 });
  if (q.isPending) return <Spinner label="Loading metrics" />;
  if (q.error) return <ErrorText error={q.error} />;
  const r = q.data;
  return (
    <div className="space-y-4">
      {!r.metricsAvailable && <p className="text-sm text-warn">metrics-server isn't installed in this cluster, so CPU and memory use are unknown.</p>}
      <div className="grid gap-4 sm:grid-cols-3">
        <Card title="CPU">
          <p className="text-sm">
            {(r.cpuMillis / 1000).toFixed(2)} of {(r.cpuLimitMillis / 1000).toFixed(1)} cores
          </p>
          <Meter value={r.cpuMillis} max={r.cpuLimitMillis} label="CPU use" />
        </Card>
        <Card title="Memory">
          <p className="text-sm">
            {bytes(r.memoryBytes)} of {bytes(r.memoryLimitBytes)}
          </p>
          <Meter value={r.memoryBytes} max={r.memoryLimitBytes} label="Memory use" />
        </Card>
        {r.netRxBytesPerSec !== undefined && (
          <Card title="Network">
            <p className="text-sm">
              ↓ {bytes(r.netRxBytesPerSec)}/s · ↑ {bytes(r.netTxBytesPerSec ?? 0)}/s
            </p>
          </Card>
        )}
      </div>
      {r.processes && <Card title="Processes">
        <div className="overflow-x-auto">
          <table className="w-full text-left text-sm">
            <thead className="text-xs text-muted">
              <tr>
                {["PID", "User", "CPU %", "Memory", "Command"].map((h) => (
                  <th key={h} scope="col" className="px-2 py-1 font-medium">
                    {h}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody className="divide-y divide-border">
              {r.processes.map((p) => (
                <tr key={p.pid}>
                  <td className="px-2 py-1">{p.pid}</td>
                  <td className="px-2 py-1">{p.user}</td>
                  <td className="px-2 py-1">{p.cpuPercent.toFixed(1)}</td>
                  <td className="px-2 py-1">{bytes(p.memoryBytes)}</td>
                  <td className="px-2 py-1 font-mono text-xs break-all">{p.command}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </Card>}
    </div>
  );
}

export function StorageTab({ workspace, room, canOperate }: { workspace: string; room: string; canOperate: boolean }) {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["storage", workspace, room], queryFn: () => api().getStorage(workspace, room) });
  const prune = useMutation({
    mutationFn: (k: PruneKind) => api().prune(workspace, room, k),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["storage", workspace, room] }),
  });
  if (q.isPending) return <Spinner label="Loading storage" />;
  if (q.error) return <ErrorText error={q.error} />;
  const s = q.data;
  const pruneButton = (k: PruneKind, label: string) =>
    canOperate && (
      <Button onClick={() => prune.mutate(k)} busy={prune.isPending && prune.variables === k}>
        {label}
      </Button>
    );
  return (
    <div className="space-y-4">
      {s.quotaEnforced === false && (
        <p className="rounded-xs border border-warn/40 bg-warn/10 p-3 text-sm">
          This StorageClass doesn't enforce volume sizes. Quotas are soft: the janitor prunes and you get warnings at 80% and 90%.
        </p>
      )}
      <Card title="Volumes">
        <ul className="space-y-3">
          {s.volumes.map((v) => (
            <li key={v.name}>
              <div className="flex justify-between text-sm">
                <span className="font-medium">{v.name}</span>
                <span>
                  {bytes(v.usedBytes)} of {bytes(v.capacityBytes)} ({percent(v.usedBytes, v.capacityBytes)}%)
                </span>
              </div>
              <Meter value={v.usedBytes} max={v.capacityBytes} label={`${v.name} volume use`} />
            </li>
          ))}
        </ul>
      </Card>
      {prune.data && (
        <p role="status" className="text-sm text-ok">
          Reclaimed {bytes(prune.data.reclaimedBytes)}.
        </p>
      )}
      {prune.data?.output && (
        <details className="text-sm">
          <summary className="cursor-pointer text-muted">Engine output</summary>
          <pre className="mt-1 max-h-64 overflow-auto rounded-xs bg-surface-2 p-2 font-mono text-xs whitespace-pre-wrap" data-testid="prune-output">
            {prune.data.output}
          </pre>
        </details>
      )}
      <ErrorText error={prune.error} />
      {canOperate && (
        <div className="flex flex-wrap gap-2">
          {pruneButton("containers", "Prune stopped containers")}
          {pruneButton("images", "Prune unused images")}
          {pruneButton("volumes", "Prune unused volumes")}
          {pruneButton("buildCache", "Prune build cache")}
          {pruneButton("all", "Prune everything unused")}
        </div>
      )}
      {s.images && <EngineList title="Images" items={s.images} />}
      {s.containers && <EngineList title="Containers" items={s.containers} />}
      {s.engineVolumes && <EngineList title="Engine volumes" items={s.engineVolumes} />}
      <Card title="Shared directories">
        {s.sharedDirs.length === 0 ? (
          <p className="text-sm text-muted">None.</p>
        ) : (
          <ul className="text-sm">
            {s.sharedDirs.map((d) => (
              <li key={d.name}>
                {d.name} {d.readOnly && <Badge>read-only</Badge>}
              </li>
            ))}
          </ul>
        )}
      </Card>
    </div>
  );
}

function EngineList({ title, items }: { title: string; items: EngineItem[] }) {
  return (
    <Card title={`${title} (${items.length})`}>
      {items.length === 0 ? (
        <p className="text-sm text-muted">None.</p>
      ) : (
        <ul className="divide-y divide-border text-sm">
          {items.map((i) => (
            <li key={i.id} className="flex flex-wrap justify-between gap-2 py-1">
              <span className="font-mono text-xs break-all">{i.name}</span>
              <span className="text-muted">
                {bytes(i.sizeBytes)} · {i.detail}
              </span>
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}

export function LogsTab({ workspace, room }: { workspace: string; room: string }) {
  const [lines, setLines] = useState<string[]>([]);
  const [component, setComponent] = useState<LogComponent | "">("");
  const [follow, setFollow] = useState(true);
  const box = useRef<HTMLDivElement>(null);
  useEffect(() => {
    setLines([]);
    const ctl = new AbortController();
    void consume(
      (s) => api().watchLogs(workspace, room, component, s),
      (l) => setLines((old) => [...old.slice(-4999), l.text]),
      ctl.signal,
    );
    return () => ctl.abort();
  }, [workspace, room, component]);
  useEffect(() => {
    if (follow && box.current) box.current.scrollTop = box.current.scrollHeight;
  }, [lines.length, follow]);
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-2">
        <Select aria-label="Component" value={component} onChange={(e) => setComponent(e.target.value as LogComponent | "")} className="w-auto">
          <option value="">All components</option>
          <option value="supervisor">supervisor</option>
          <option value="cli">CLI</option>
          <option value="engine">engine</option>
          <option value="janitor">janitor</option>
        </Select>
        <Checkbox label="Follow" checked={follow} onChange={(e) => setFollow(e.target.checked)} />
      </div>
      <div ref={box} role="log" aria-label="Logs" className="h-[60vh] min-h-64 overflow-auto rounded-xs border border-border bg-surface-2 p-2 font-mono text-xs">
        {lines.length === 0 && <Empty>No log lines yet.</Empty>}
        {lines.map((l, i) => (
          <div key={i} className="whitespace-pre-wrap break-all">
            {l}
          </div>
        ))}
      </div>
    </div>
  );
}
