import { Link } from "@tanstack/react-router";
import { useState } from "react";
import type { Presence, RoomSummary } from "@/api/types";
import { PresenceBadge, presenceInfo } from "@/components/Presence";
import { Badge, Empty, Input, Meter, PageHeader, Select, Spinner, cx } from "@/components/ui";
import { bytes, percent, usd } from "@/lib/format";
import { useFleet } from "@/lib/live";

const VIEW_KEY = "swarm-fleet-view";

function readView(): "cards" | "table" {
  try {
    return localStorage.getItem(VIEW_KEY) === "table" ? "table" : "cards";
  } catch {
    return "cards";
  }
}

/** Every room, live (§8.4). Waiting on a human sorts first. */
export function FleetPage() {
  const { rooms } = useFleet();
  const [view, setView] = useState(readView);
  const [q, setQ] = useState("");
  const [ws, setWs] = useState("");
  const [presence, setPresence] = useState<Presence | "">("");
  const switchView = (v: "cards" | "table") => {
    setView(v);
    try {
      localStorage.setItem(VIEW_KEY, v);
    } catch {
      /* ignore */
    }
  };
  const workspaces = [...new Set(rooms?.map((r) => r.workspace))];
  const shown = (rooms ?? []).filter(
    (r) =>
      (!ws || r.workspace === ws) &&
      (!presence || r.presence === presence) &&
      (!q || `${r.name} ${r.currentTask} ${r.role} ${r.model}`.toLowerCase().includes(q.toLowerCase())),
  );
  const counts = (rooms ?? []).reduce<Partial<Record<Presence, number>>>((a, r) => ({ ...a, [r.presence]: (a[r.presence] ?? 0) + 1 }), {});

  return (
    <div>
      <PageHeader
        title="Fleet"
        subtitle={rooms ? `${rooms.length} rooms` + (counts.waitingOnHuman ? `, ${counts.waitingOnHuman} waiting on you` : "") : undefined}
        actions={
          <div role="group" aria-label="View" className="flex rounded-md border border-border">
            {(["cards", "table"] as const).map((v) => (
              <button
                key={v}
                type="button"
                aria-pressed={view === v}
                onClick={() => switchView(v)}
                className={cx("min-h-10 px-3 text-sm capitalize", view === v ? "bg-surface-2 font-medium" : "text-muted")}
              >
                {v}
              </button>
            ))}
          </div>
        }
      />
      <div className="mb-4 grid gap-2 sm:grid-cols-3">
        <Input aria-label="Search rooms" placeholder="Search name, task, role, model" value={q} onChange={(e) => setQ(e.target.value)} />
        <Select aria-label="Workspace" value={ws} onChange={(e) => setWs(e.target.value)}>
          <option value="">All workspaces</option>
          {workspaces.map((w) => (
            <option key={w}>{w}</option>
          ))}
        </Select>
        <Select aria-label="Presence" value={presence} onChange={(e) => setPresence(e.target.value as Presence | "")}>
          <option value="">Any presence</option>
          {(Object.keys(presenceInfo) as Presence[]).map((p) => (
            <option key={p} value={p}>
              {presenceInfo[p].label} {counts[p] ? `(${counts[p]})` : ""}
            </option>
          ))}
        </Select>
      </div>
      {!rooms && <Spinner label="Connecting to the fleet" />}
      {rooms && rooms.length === 0 && (
        <Empty>
          No rooms yet.{" "}
          <Link to="/workspaces/new" className="text-accent underline">
            Create a workspace
          </Link>
          .
        </Empty>
      )}
      {rooms && rooms.length > 0 && shown.length === 0 && <Empty>No room matches.</Empty>}
      {view === "cards" ? (
        <ul className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3" aria-label="Rooms">
          {shown.map((r) => (
            <RoomCard key={`${r.workspace}/${r.name}`} r={r} />
          ))}
        </ul>
      ) : (
        shown.length > 0 && <RoomTable rooms={shown} />
      )}
    </div>
  );
}

function RoomLink({ r, children, className }: { r: RoomSummary; children: React.ReactNode; className?: string }) {
  return (
    <Link to="/rooms/$workspace/$room" params={{ workspace: r.workspace, room: r.name }} search={{ tab: "agent" }} className={className}>
      {children}
    </Link>
  );
}

function RoomCard({ r }: { r: RoomSummary }) {
  return (
    <li
      data-testid="room-card"
      data-room={r.name}
      data-presence={r.presence}
      className={cx("rounded-lg border bg-surface p-3", r.presence === "waitingOnHuman" ? "border-warn" : r.presence === "crashed" ? "border-bad" : "border-border")}
    >
      <div className="flex items-start justify-between gap-2">
        <div className="min-w-0">
          <RoomLink r={r} className="font-semibold hover:underline">
            {r.displayName}
          </RoomLink>
          <p className="truncate text-xs text-muted">
            {r.workspace} · {r.role}
            {r.group && ` · ${r.group}`}
          </p>
        </div>
        <PresenceBadge presence={r.presence} />
      </div>
      <p className="mt-2 line-clamp-2 min-h-10 text-sm">{r.currentTask || <span className="text-muted">No current task</span>}</p>
      <div className="mt-2 flex flex-wrap gap-1">
        <Badge>{r.cli === "claude" ? "Claude Code" : "ore"}</Badge>
        <Badge>{r.model}</Badge>
        {r.phase !== "Running" && <Badge tone={r.phase === "Failed" ? "bad" : "info"}>{r.phase}</Badge>}
        {r.updateAvailable && <Badge tone="info">Update available</Badge>}
        {r.pendingApprovals > 0 && (
          <Link to="/approvals">
            <Badge tone="warn">{r.pendingApprovals} approval{r.pendingApprovals > 1 && "s"}</Badge>
          </Link>
        )}
      </div>
      <dl className="mt-3 grid grid-cols-2 gap-2 text-xs">
        <div>
          <dt className="text-muted">Spend</dt>
          <dd>
            {usd(r.spendUSD)}
            {r.budgetUSD > 0 && <span className="text-muted"> / {usd(r.budgetUSD)}</span>}
          </dd>
          {r.budgetUSD > 0 && <Meter value={r.spendUSD} max={r.budgetUSD} label={`${r.name} budget used`} />}
        </div>
        <div>
          <dt className="text-muted">Disk</dt>
          <dd>
            {bytes(r.diskUsedBytes)} ({percent(r.diskUsedBytes, r.diskCapacityBytes)}%)
          </dd>
          <Meter value={r.diskUsedBytes} max={r.diskCapacityBytes} label={`${r.name} disk used`} />
        </div>
      </dl>
    </li>
  );
}

function RoomTable({ rooms }: { rooms: RoomSummary[] }) {
  return (
    <div className="overflow-x-auto rounded-lg border border-border bg-surface">
      <table className="w-full text-left text-sm">
        <caption className="sr-only">Rooms</caption>
        <thead className="bg-surface-2 text-xs text-muted">
          <tr>
            {["Room", "Presence", "CLI", "Model", "Current task", "Spend", "Disk"].map((h) => (
              <th key={h} scope="col" className="px-3 py-2 font-medium">
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className="divide-y divide-border">
          {rooms.map((r) => (
            <tr key={`${r.workspace}/${r.name}`} data-testid="room-row" data-room={r.name}>
              <td className="px-3 py-2">
                <RoomLink r={r} className="font-medium hover:underline">
                  {r.name}
                </RoomLink>
                <span className="block text-xs text-muted">{r.workspace}</span>
              </td>
              <td className="px-3 py-2">
                <PresenceBadge presence={r.presence} />
              </td>
              <td className="px-3 py-2">{r.cli}</td>
              <td className="px-3 py-2">{r.model}</td>
              <td className="max-w-xs truncate px-3 py-2">{r.currentTask}</td>
              <td className="px-3 py-2 whitespace-nowrap">{usd(r.spendUSD)}</td>
              <td className="px-3 py-2 whitespace-nowrap">
                {bytes(r.diskUsedBytes)} ({percent(r.diskUsedBytes, r.diskCapacityBytes)}%)
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
