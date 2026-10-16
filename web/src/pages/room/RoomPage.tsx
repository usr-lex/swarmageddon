import { useQuery } from "@tanstack/react-query";
import { Link, useNavigate, useParams, useSearch } from "@tanstack/react-router";
import { Tabs } from "radix-ui";
import { api } from "@/api";
import { useMe } from "@/components/Shell";
import { PresenceBadge } from "@/components/Presence";
import { Badge, ErrorText, Spinner } from "@/components/ui";
import { useFleet } from "@/lib/live";
import { ActivityTab, AgentTab, LogsTab, ResourcesTab, StorageTab } from "./tabs";
import { Lifecycle } from "./Lifecycle";
import { TerminalView } from "@/components/TerminalView";
import { DisplayView } from "@/components/DisplayView";

export const roomTabs = ["agent", "shell", "display", "activity", "resources", "storage", "logs"] as const;
export type RoomTab = (typeof roomTabs)[number];

const tabLabels: Record<RoomTab, string> = {
  agent: "Agent",
  shell: "Shell",
  display: "Display",
  activity: "Activity",
  resources: "Resources",
  storage: "Storage",
  logs: "Logs",
};
const operatorOnly: RoomTab[] = ["shell", "display"];

export function RoomPage() {
  const { workspace, room } = useParams({ from: "/rooms/$workspace/$room" });
  const { tab } = useSearch({ from: "/rooms/$workspace/$room" });
  const navigate = useNavigate({ from: "/rooms/$workspace/$room" });
  const me = useMe();
  const canOperate = me.data?.role === "admin" || me.data?.role === "operator";
  const detail = useQuery({ queryKey: ["room", workspace, room], queryFn: () => api().getRoom(workspace, room) });
  const { rooms } = useFleet();
  // Presence and phase come live from the fleet stream; the rest from GetRoom.
  const live = rooms?.find((r) => r.workspace === workspace && r.name === room) ?? detail.data?.summary;
  const tabs = roomTabs.filter((t) => canOperate || !operatorOnly.includes(t));
  const current = tabs.includes(tab) ? tab : "agent";

  if (detail.isPending) return <Spinner label="Loading room" />;
  if (detail.error) return <ErrorText error={detail.error} />;
  const s = live!;
  return (
    <div className="gridded -m-4 min-h-full space-y-4 p-4">
      <nav aria-label="Breadcrumb" className="text-sm text-muted">
        <Link to="/fleet" className="hover:underline">
          Fleet
        </Link>{" "}
        / {workspace} / <span className="text-fg">{room}</span>
      </nav>
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="flex flex-wrap items-center gap-2 text-2xl font-semibold">
            {s.displayName} <PresenceBadge presence={s.presence} />
          </h1>
          <p className="text-sm text-muted">
            {s.role} · {s.cli === "claude" ? "Claude Code" : "ore"} · {s.model} · {s.phase}
            {s.group && ` · group ${s.group}`}
          </p>
          {s.currentTask && <p className="mt-1 text-sm">{s.currentTask}</p>}
        </div>
        <div className="flex flex-wrap gap-1">
          {s.updateAvailable && <Badge tone="info">Update available</Badge>}
          {s.budgetUSD > 0 && s.spendUSD >= s.budgetUSD && <Badge tone="bad">Budget exhausted</Badge>}
        </div>
      </div>
      {canOperate && <Lifecycle room={s} />}
      <Tabs.Root value={current} onValueChange={(v) => navigate({ search: { tab: v as RoomTab }, replace: true })}>
        <Tabs.List aria-label="Room" className="flex gap-1 overflow-x-auto border-b border-border">
          {tabs.map((t) => (
            <Tabs.Trigger
              key={t}
              value={t}
              className="min-h-10 px-3 text-sm whitespace-nowrap text-muted data-[state=active]:border-b-2 data-[state=active]:border-accent data-[state=active]:font-medium data-[state=active]:text-fg"
            >
              {tabLabels[t]}
            </Tabs.Trigger>
          ))}
        </Tabs.List>
        <Tabs.Content value={current} className="pt-4">
          {current === "agent" && <AgentTab workspace={workspace} room={room} canOperate={canOperate} />}
          {current === "shell" && <TerminalView workspace={workspace} room={room} mode="shell" readOnly={false} />}
          {current === "display" && <DisplayView workspace={workspace} room={room} />}
          {current === "activity" && <ActivityTab detail={detail.data} live={s} />}
          {current === "resources" && <ResourcesTab workspace={workspace} room={room} />}
          {current === "storage" && <StorageTab workspace={workspace} room={room} canOperate={canOperate} />}
          {current === "logs" && <LogsTab workspace={workspace} room={room} />}
        </Tabs.Content>
      </Tabs.Root>
    </div>
  );
}
