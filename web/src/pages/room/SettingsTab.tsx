import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useState } from "react";
import { api } from "@/api";
import { isEmptySettings, type RoomSettings } from "@/api/settings";
import { EffectiveView, SettingsEditor } from "@/components/SettingsEditor";
import { Badge, Button, Card, ErrorText, Spinner } from "@/components/ui";

/** Inherited levels, read-only here; they're changed in the workspace wizard. */
function LevelSummary({ title, s }: { title: string; s: RoomSettings }) {
  if (isEmptySettings(s)) return <p className="text-sm text-muted">{title}: nothing set.</p>;
  return (
    <div className="text-sm">
      <p className="label">{title}</p>
      <ul className="font-mono text-xs">
        {Object.entries(s.env).map(([k, v]) => (
          <li key={k}>
            {k}={v}
          </li>
        ))}
        {s.secretEnv.map((e) => (
          <li key={e.name}>
            {e.name} ← {e.secret.name}/{e.secret.key}
          </li>
        ))}
        {Object.entries(s.mcpServers).map(([k, v]) => (
          <li key={k}>mcp {k}{v.disabled ? " (removed)" : ""}</li>
        ))}
        {s.network && (
          <li>
            network {s.network.mode || "inherit"}
            {s.network.domains.length ? ` +${s.network.domains.join(", ")}` : ""}
            {s.network.packageManagers !== undefined && ` · package managers ${s.network.packageManagers ? "on" : "off"}`}
          </li>
        )}
      </ul>
    </div>
  );
}

/** The room's layered settings (§6.6): workspace and role levels, its own level, and the merged result. */
export function SettingsTab({ workspace, room, canOperate }: { workspace: string; room: string; canOperate: boolean }) {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["roomSettings", workspace, room], queryFn: () => api().getRoomSettings(workspace, room) });
  const [own, setOwn] = useState<RoomSettings>();
  const save = useMutation({
    mutationFn: (s: RoomSettings) => api().updateRoomSettings(workspace, room, s),
    onSuccess: async () => {
      setOwn(undefined);
      await qc.invalidateQueries({ queryKey: ["roomSettings", workspace, room] });
    },
  });
  if (q.isPending) return <Spinner label="Loading settings" />;
  if (q.error) return <ErrorText error={q.error} />;
  const v = q.data;
  const vols = Object.entries(v.sharedVolumes);
  return (
    <div className="grid gap-4 lg:grid-cols-[1fr_20rem]">
      <div className="space-y-4">
        <Card
          title="Inherited"
          actions={
            canOperate && (
              <Link to="/workspaces/$workspace/edit" params={{ workspace }} className="text-sm text-accent hover:underline">
                Change in the workspace
              </Link>
            )
          }
        >
          <div className="space-y-2">
            <LevelSummary title="Workspace" s={v.workspace} />
            <LevelSummary title={`Role ${v.effective.role}`} s={v.role} />
          </div>
        </Card>
        <Card title="This room">
          {canOperate ? (
            <>
              <SettingsEditor value={own ?? v.room} onChange={setOwn} workspace={workspace} workspaceExists level="room" />
              <ErrorText error={save.error} />
              <div className="mt-3 flex justify-end gap-2">
                <Button disabled={!own} onClick={() => setOwn(undefined)}>
                  Discard
                </Button>
                <Button variant="primary" disabled={!own} busy={save.isPending} onClick={() => own && save.mutate(own)}>
                  Save
                </Button>
              </div>
            </>
          ) : (
            <LevelSummary title="Room" s={v.room} />
          )}
        </Card>
      </div>
      <div className="space-y-4">
        <Card title="Effective">
          <EffectiveView effective={v.effective} />
        </Card>
        <Card title="Shared volumes">
          {vols.length === 0 ? (
            <p className="text-sm text-muted">None mounted.</p>
          ) : (
            <ul className="space-y-1 text-sm">
              {vols.map(([n, m]) => (
                <li key={n} className="flex items-center justify-between">
                  <span className="font-mono">/shared/{n}</span>
                  <Badge tone={m === "rw" ? "info" : "muted"}>{m === "rw" ? "read-write" : "read-only"}</Badge>
                </li>
              ))}
            </ul>
          )}
        </Card>
      </div>
    </div>
  );
}
