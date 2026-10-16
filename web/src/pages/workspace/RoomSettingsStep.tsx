import { useState } from "react";
import { effectiveFor, emptySettings, type RoomSettings, type SettingLevel, type SharedVolumeDraft } from "@/api/settings";
import { EffectiveView, SettingsEditor } from "@/components/SettingsEditor";
import { Button, Card, Checkbox, Input, Select, cx } from "@/components/ui";
import type { DraftProps } from "./WorkspaceWizard";

/**
 * Layered room settings (§6.6): edit the workspace, a role or a room level,
 * and see what one room ends up with.
 */
export function RoomSettingsStep({ draft, set, existing }: DraftProps) {
  const roles = [...new Set(draft.rooms.map((r) => r.role))].sort();
  const [level, setLevel] = useState<SettingLevel>("workspace");
  const [role, setRole] = useState(roles[0] ?? "");
  const [roomName, setRoomName] = useState(draft.rooms[0]?.name ?? "");
  const room = draft.rooms.find((r) => r.name === roomName) ?? draft.rooms[0];

  const value: RoomSettings =
    level === "workspace" ? draft.roomDefaults : level === "role" ? (draft.roleDefaults[role] ?? emptySettings()) : (room?.settings ?? emptySettings());
  const onChange = (s: RoomSettings) => {
    if (level === "workspace") set({ ...draft, roomDefaults: s });
    else if (level === "role") set({ ...draft, roleDefaults: { ...draft.roleDefaults, [role]: s } });
    else if (room) set({ ...draft, rooms: draft.rooms.map((r) => (r.name === room.name ? { ...r, settings: s } : r)) });
  };
  // Show the merged view for the room being edited, or the first room of the role.
  const previewRoom = level === "room" ? room : level === "role" ? (draft.rooms.find((r) => r.role === role) ?? room) : room;
  const effective = previewRoom && effectiveFor(previewRoom.name, previewRoom.role, draft.roomDefaults, draft.roleDefaults[previewRoom.role], previewRoom.settings);
  const editorKey = `${level}/${level === "role" ? role : level === "room" ? room?.name : ""}`;

  return (
    <div className="grid gap-4 lg:grid-cols-[1fr_20rem]">
      <Card title="Room settings">
        <p className="mb-3 text-sm text-muted">
          Environment, secrets, MCP servers and network access for the rooms. Set them for the whole workspace, a role or one room: later levels
          override earlier ones by name, domains add up, and an MCP server can be removed again further down.
        </p>
        <div role="tablist" aria-label="Settings level" className="mb-3 flex flex-wrap items-center gap-2">
          {(["workspace", "role", "room"] as const).map((l) => (
            <button
              key={l}
              type="button"
              role="tab"
              aria-selected={level === l}
              onClick={() => setLevel(l)}
              className={cx("min-h-10 border px-3 text-sm capitalize", level === l ? "border-accent bg-accent text-accent-fg" : "border-border bg-surface")}
            >
              {l}
            </button>
          ))}
          {level === "role" && (
            <Select aria-label="Role" className="!w-44" value={role} onChange={(e) => setRole(e.target.value)}>
              {roles.map((r) => (
                <option key={r}>{r}</option>
              ))}
            </Select>
          )}
          {level === "room" && (
            <Select aria-label="Room" className="!w-44" value={room?.name ?? ""} onChange={(e) => setRoomName(e.target.value)}>
              {draft.rooms.map((r) => (
                <option key={r.name}>{r.name}</option>
              ))}
            </Select>
          )}
        </div>
        {level !== "workspace" && (level === "role" ? !role : !room) ? (
          <p className="text-sm text-muted">Add agents first.</p>
        ) : (
          <SettingsEditor key={editorKey} value={value} onChange={onChange} workspace={draft.name} workspaceExists={!!existing} level={level} />
        )}
      </Card>
      <Card title="Effective">{effective ? <EffectiveView effective={effective} /> : <p className="text-sm text-muted">No rooms yet.</p>}</Card>
    </div>
  );
}

/** Shared volumes (§6.5): ReadWriteMany claims mounted into rooms by selector. */
export function SharedVolumesStep({ draft, set }: DraftProps) {
  const vols = draft.sharedVolumes;
  const put = (i: number, v: SharedVolumeDraft) => set({ ...draft, sharedVolumes: vols.map((x, j) => (j === i ? v : x)) });
  const selectors = ["everyone", ...new Set(draft.rooms.map((r) => `role:${r.role}`)), ...draft.groups.map((g) => `group:${g}`), ...draft.rooms.map((r) => `room:${r.name}`)];
  return (
    <Card title="Shared volumes">
      <p className="mb-3 text-sm text-muted">Folders several rooms mount at /shared/&lt;name&gt;. Read-only for most rooms is a good default; let the producers write.</p>
      <datalist id="mount-selectors">
        {selectors.map((s) => (
          <option key={s} value={s} />
        ))}
      </datalist>
      <div className="space-y-3">
        {vols.map((v, i) => (
          <div key={i} className="space-y-2 border border-border p-3" data-testid={`volume-${v.name}`}>
            <div className="flex flex-wrap gap-2">
              <Input aria-label="Volume name" className="!w-44 font-mono" value={v.name} onChange={(e) => put(i, { ...v, name: e.target.value })} />
              <label className="flex items-center gap-1 text-sm">
                <Input aria-label="Size in GiB" type="number" min={1} className="!w-24" value={v.sizeGi} onChange={(e) => put(i, { ...v, sizeGi: Number(e.target.value) })} /> GiB
              </label>
              <Input aria-label="StorageClass" className="!w-44" placeholder="StorageClass (default RWX)" value={v.storageClass} onChange={(e) => put(i, { ...v, storageClass: e.target.value })} />
              <Button variant="ghost" className="ml-auto" onClick={() => set({ ...draft, sharedVolumes: vols.filter((_, j) => j !== i) })}>
                Remove
              </Button>
            </div>
            <ul className="space-y-1">
              {v.mounts.map((m, k) => (
                <li key={k} className="flex flex-wrap items-center gap-2">
                  <Input
                    aria-label="Mounted by"
                    list="mount-selectors"
                    className="!w-56 font-mono"
                    value={m.select}
                    onChange={(e) => put(i, { ...v, mounts: v.mounts.map((x, j) => (j === k ? { ...x, select: e.target.value } : x)) })}
                  />
                  <Checkbox label="read-only" checked={m.readOnly} onChange={(e) => put(i, { ...v, mounts: v.mounts.map((x, j) => (j === k ? { ...x, readOnly: e.target.checked } : x)) })} />
                  <Button variant="ghost" aria-label="Remove mount" onClick={() => put(i, { ...v, mounts: v.mounts.filter((_, j) => j !== k) })}>
                    ×
                  </Button>
                </li>
              ))}
            </ul>
            <Button variant="ghost" onClick={() => put(i, { ...v, mounts: [...v.mounts, { select: "everyone", readOnly: true }] })}>
              + Mount
            </Button>
          </div>
        ))}
        <Button onClick={() => set({ ...draft, sharedVolumes: [...vols, { name: `shared-${vols.length + 1}`, sizeGi: 20, storageClass: "", mounts: [{ select: "everyone", readOnly: true }] }] })}>
          Add shared volume
        </Button>
      </div>
    </Card>
  );
}
