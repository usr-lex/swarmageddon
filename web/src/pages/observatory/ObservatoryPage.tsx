import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, Navigate, useParams } from "@tanstack/react-router";
import { useEffect, useMemo, useState } from "react";
import { api } from "@/api";
import type { ObsWindow } from "@/api/observatory";
import { usd } from "@/lib/format";
import { useFleet } from "@/lib/live";
import { AxesPanel } from "./Axes";
import { ChordChart } from "./Chord";
import { Panel, Segmented, windows } from "./common";
import { ForceGraph } from "./ForceGraph";
import { Heatmap } from "./Heatmap";
import { NowTable, StatusReport, Timeline } from "./panels";
import { Throughput } from "./Throughput";

const HEAT_DAYS = 182;
const TP_DAYS = 30;

/** The swarm observatory (§8.7): the workspace's home page. */
export function ObservatoryPage() {
  const { workspace } = useParams({ from: "/w/$workspace" });
  const [win, setWin] = useState<ObsWindow>("24h");
  const o = api().observatory;
  const qc = useQueryClient();
  const edges = useQuery({ queryKey: ["obs", "edges", workspace, win], queryFn: () => o.edges(workspace, win), placeholderData: (p) => p, refetchInterval: 30_000 });
  const now = useQuery({ queryKey: ["obs", "now", workspace], queryFn: () => o.now(workspace), refetchInterval: 15_000 });
  const axes = useQuery({ queryKey: ["obs", "axes", workspace, win], queryFn: () => o.axes(workspace, win), placeholderData: (p) => p });
  const heat = useQuery({ queryKey: ["obs", "heat", workspace], queryFn: () => o.heatmap(workspace, HEAT_DAYS), staleTime: 5 * 60_000 });
  const tp = useQuery({ queryKey: ["obs", "tp", workspace], queryFn: () => o.throughput(workspace, TP_DAYS), staleTime: 5 * 60_000 });
  const { rooms } = useFleet();

  // Presence changes from the fleet stream refresh the Now panel.
  const presenceKey = rooms?.filter((r) => r.workspace === workspace).map((r) => `${r.name}:${r.presence}`).join(",");
  useEffect(() => {
    if (presenceKey) void qc.invalidateQueries({ queryKey: ["obs", "now", workspace] });
  }, [presenceKey, qc, workspace]);

  const groupOf = useMemo(() => new Map(edges.data?.agents.map((a) => [a.name, a.group]) ?? []), [edges.data]);
  const groups = useMemo(() => [...new Set(edges.data?.agents.map((a) => a.group))].sort(), [edges.data]);
  const today = tp.data?.[tp.data.length - 1];
  const sum = (r?: Record<string, number>) => Object.values(r ?? {}).reduce((a, b) => a + b, 0);
  const ws = rooms?.filter((r) => r.workspace === workspace) ?? [];
  const waiting = ws.filter((r) => r.presence === "waitingOnHuman").length;

  return (
    <div className="gridded -m-4 min-h-full p-4">
      <div className="mb-3 flex flex-wrap items-end justify-between gap-3">
        <div>
          <p className="label">Observatory</p>
          <h1 className="flex items-center gap-2 text-2xl font-semibold tracking-tight">
            {workspace}
            <span className="label inline-flex items-center gap-1 !text-ok">
              <span aria-hidden className="live-dot inline-block size-1.5 rounded-full bg-ok" /> live
            </span>
          </h1>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <span className="label">Window</span>
          <Segmented label="Window" value={win} options={windows} onChange={setWin} />
          <Link to="/workspaces/$workspace/edit" params={{ workspace }} className="label min-h-8 content-center border border-border px-2 hover:!text-fg">
            Configure
          </Link>
        </div>
      </div>

      <dl className="panel mb-3 grid grid-cols-2 divide-border sm:grid-cols-3 lg:grid-cols-6 lg:divide-x" data-testid="stats">
        <Stat label="Agents" value={ws.length || "–"} />
        <Stat label="Working" value={ws.filter((r) => r.presence === "working").length} />
        <Stat label="Waiting on you" value={waiting} alert={waiting > 0} to="/approvals" />
        <Stat label="PRs merged today" value={today ? sum(today.prsMerged) : "–"} />
        <Stat label="CI failures today" value={today ? today.ciFailures : "–"} alert={!!today?.ciFailures} />
        <Stat label="Spend today" value={today ? usd(sum(today.spendUSD)) : "–"} />
      </dl>

      <div className="grid grid-cols-1 gap-3 lg:grid-cols-12">
        <Panel id="panel-status" label="Swarm status" className="lg:col-span-5">
          <StatusReport workspace={workspace} />
        </Panel>
        <Panel id="panel-now" label="Now" meta={`${ws.length} agents`} className="lg:col-span-7">
          <NowTable workspace={workspace} rows={now.data} />
        </Panel>
        <Panel id="panel-chord" label="Who talks to whom" meta={`messages · ${win}`} className={`lg:col-span-5 ${edges.isPlaceholderData ? "opacity-60" : ""}`}>
          <ChordChart workspace={workspace} window={win} edges={edges.data} />
        </Panel>
        <Panel id="panel-graph" label="Handoff graph" meta={`messages + forge · ${win}`} className={`lg:col-span-7 ${edges.isPlaceholderData ? "opacity-60" : ""}`}>
          <ForceGraph edges={edges.data} />
        </Panel>
        <Panel id="panel-throughput" label="Throughput" meta={`${TP_DAYS} days`} className="lg:col-span-7">
          <Throughput days={tp.data} groupOf={groupOf} groups={groups} />
        </Panel>
        <Panel id="panel-timeline" label="Timeline" meta="live" className="lg:col-span-5">
          <Timeline workspace={workspace} agents={[...groupOf.keys()].sort()} />
        </Panel>
        <Panel id="panel-axes" label="Activity axes" meta={win} className="lg:col-span-12">
          <AxesPanel data={axes.data} />
        </Panel>
        <Panel id="panel-heatmap" label="Contribution heatmap" meta={`${HEAT_DAYS} days`} className="lg:col-span-12">
          <Heatmap cells={heat.data} days={HEAT_DAYS} />
        </Panel>
      </div>
    </div>
  );
}

function Stat({ label, value, alert, to }: { label: string; value: React.ReactNode; alert?: boolean; to?: "/approvals" }) {
  const body = (
    <>
      <dt className="label">{label}</dt>
      <dd className={`font-mono text-2xl tabular-nums ${alert ? "text-alert" : ""}`}>{value}</dd>
    </>
  );
  return to ? (
    <Link to={to} className="block px-3 py-2 hover:bg-surface-2">
      {body}
    </Link>
  ) : (
    <div className="px-3 py-2">{body}</div>
  );
}

/** "/" lands on the first workspace's observatory, or the fleet when there is none. */
export function HomeRedirect() {
  const q = useQuery({ queryKey: ["workspaces"], queryFn: () => api().listWorkspaces() });
  if (q.isPending) return null;
  const first = q.data?.[0]?.name;
  return first ? <Navigate to="/w/$workspace" params={{ workspace: first }} replace /> : <Navigate to="/fleet" replace />;
}
