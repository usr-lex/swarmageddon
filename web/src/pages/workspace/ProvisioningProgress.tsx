import { Link } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { api } from "@/api";
import { consume } from "@/api/stream";
import type { RoomProgress } from "@/api/types";
import { Badge, Card, Spinner } from "@/components/ui";

const conditionLabels: Record<string, string> = {
  ForgeAccountReady: "Forgejo account",
  KeysReady: "Keys",
  GatewayKeyReady: "Gateway key",
  MessagingIdentityReady: "Messaging identity",
  PodReady: "Pod",
};

/** Per-room provisioning, from the rooms' conditions (§8.3). */
export function ProvisioningProgress({ workspace }: { workspace: string }) {
  const [rooms, setRooms] = useState<RoomProgress[]>();
  useEffect(() => {
    const ctl = new AbortController();
    void consume((s) => api().watchProvisioning(workspace, s), setRooms, ctl.signal);
    return () => ctl.abort();
  }, [workspace]);
  const done = rooms?.length && rooms.every((r) => r.phase !== "Provisioning");
  return (
    <div className="space-y-4">
      <h1 className="text-2xl font-semibold">Provisioning {workspace}</h1>
      <Card title={done ? "All rooms are up" : "Creating rooms"}>
        {!rooms && <Spinner label="Waiting for the operator" />}
        <ul className="divide-y divide-border" aria-label="Room progress">
          {rooms?.map((r) => (
            <li key={r.name} className="py-2" data-testid={`progress-${r.name}`} data-phase={r.phase}>
              <div className="flex items-center justify-between gap-2">
                <span className="font-medium">{r.name}</span>
                {r.phase === "Provisioning" ? <Spinner label="Provisioning" /> : <Badge tone={r.phase === "Failed" ? "bad" : "ok"}>{r.phase}</Badge>}
              </div>
              <ul className="mt-1 flex flex-wrap gap-1">
                {r.conditions.map((c) => (
                  <li key={c.type}>
                    <Badge tone={c.status === "True" ? "ok" : c.reason.match(/fail|error/i) ? "bad" : "muted"}>
                      {c.status === "True" ? "✓" : "…"} {conditionLabels[c.type] ?? c.type}
                    </Badge>
                  </li>
                ))}
              </ul>
            </li>
          ))}
        </ul>
      </Card>
      <Link to="/fleet" className="inline-flex min-h-10 items-center rounded-md bg-accent px-4 text-sm font-medium text-accent-fg">
        {done ? "Go to the fleet" : "Go to the fleet now"}
      </Link>
    </div>
  );
}
