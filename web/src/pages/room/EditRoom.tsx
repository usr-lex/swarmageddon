import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { api } from "@/api";
import type { CLI, ModelAccess, PlanResult, RoomDraft, RoomSummary, WorkspaceDraft } from "@/api/types";
import { Badge, Button, Checkbox, Dialog, ErrorText, Field, Input, Select, Spinner } from "@/components/ui";

type Edit = Pick<RoomDraft, "cli" | "model" | "role" | "cpu" | "memoryGi" | "modelAccess">;

/**
 * Change one room's CLI, model, role or size without the workspace wizard
 * (OpenShift report, issue 7). It edits this room's entry in the workspace
 * plan, previews it with the same check the wizard uses, and applies it.
 */
export function EditRoom({ room }: { room: RoomSummary }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <Button onClick={() => setOpen(true)}>Edit</Button>
      {open && <EditDialog room={room} onClose={() => setOpen(false)} />}
    </>
  );
}

function EditDialog({ room, onClose }: { room: RoomSummary; onClose: () => void }) {
  const qc = useQueryClient();
  const draft = useQuery({ queryKey: ["workspaceDraft", room.workspace], queryFn: () => api().getWorkspaceDraft(room.workspace) });
  const roles = useQuery({ queryKey: ["roleTemplates"], queryFn: () => api().listRoleTemplates() });
  const models = useQuery({ queryKey: ["models"], queryFn: () => api().listModels() });
  const current = draft.data?.rooms.find((r) => r.name === room.name);
  const [edit, setEdit] = useState<Edit | null>(null);
  const [now, setNow] = useState(true);
  const [preview, setPreview] = useState<PlanResult | null>(null);
  const value: Edit | undefined = edit ?? (current && { cli: current.cli, model: current.model, role: current.role, cpu: current.cpu, memoryGi: current.memoryGi, modelAccess: current.modelAccess ?? "" });
  const set = (p: Partial<Edit>) => {
    setPreview(null);
    setEdit({ ...value!, ...p });
  };
  const changed = (d: WorkspaceDraft): WorkspaceDraft => ({ ...d, rooms: d.rooms.map((r) => (r.name === room.name ? { ...r, ...value } : r)) });

  const plan = useMutation({ mutationFn: () => api().planWorkspace(changed(draft.data!)), onSuccess: setPreview });
  const apply = useMutation({
    mutationFn: async () => {
      await api().applyWorkspace(changed(draft.data!));
      if (now) await api().roomAction(room.workspace, room.name, { kind: "updateNow" });
    },
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ["room", room.workspace, room.name] });
      await qc.invalidateQueries({ queryKey: ["workspaceDraft", room.workspace] });
      onClose();
    },
  });

  const others = preview?.changes.filter((c) => c.op !== "unchanged" && !(c.kind === "Room" && c.name === room.name)) ?? [];
  const mine = preview?.changes.find((c) => c.kind === "Room" && c.name === room.name);

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()} title={`Edit ${room.name}`} description="Changes this room's entry in the workspace plan. Its home volume, keys and history are kept.">
      {draft.isLoading && <Spinner label="Loading the workspace plan" />}
      <ErrorText error={draft.error} />
      {draft.data && !current && <ErrorText error={`${room.name} isn't in the workspace plan; edit it in the workspace wizard.`} />}
      {value && (
        <div className="grid grid-cols-2 gap-3">
          <Field label="CLI">
            {(id) => (
              <Select id={id} value={value.cli} onChange={(e) => set({ cli: e.target.value as CLI })}>
                <option value="claude">Claude Code</option>
                <option value="ore">ore</option>
              </Select>
            )}
          </Field>
          <Field label="Model">
            {(id) => (
              <Select id={id} value={value.model} onChange={(e) => set({ model: e.target.value })}>
                {[...new Set([value.model, ...(models.data ?? [])])].map((m) => (
                  <option key={m} value={m}>
                    {m}
                  </option>
                ))}
              </Select>
            )}
          </Field>
          <Field label="Role">
            {(id) => (
              <Select id={id} value={value.role} onChange={(e) => set({ role: e.target.value })}>
                {[...new Set([value.role, ...(roles.data ?? []).map((r) => r.name)])].map((r) => (
                  <option key={r} value={r}>
                    {r}
                  </option>
                ))}
              </Select>
            )}
          </Field>
          <Field label="Model access">
            {(id) => (
              <Select id={id} value={value.modelAccess ?? ""} onChange={(e) => set({ modelAccess: e.target.value as ModelAccess | "" })}>
                <option value="">Workspace default ({draft.data?.modelAccess === "direct" ? "direct" : "gateway"})</option>
                <option value="gateway">Gateway</option>
                <option value="direct">Direct: the CLI's own key or login</option>
              </Select>
            )}
          </Field>
          <div className="grid grid-cols-2 gap-2">
            <Field label="CPU">{(id) => <Input id={id} type="number" min={0.1} step={0.1} value={value.cpu} onChange={(e) => set({ cpu: Number(e.target.value) })} />}</Field>
            <Field label="Memory (GiB)">{(id) => <Input id={id} type="number" min={1} step={1} value={value.memoryGi} onChange={(e) => set({ memoryGi: Number(e.target.value) })} />}</Field>
          </div>
        </div>
      )}
      {preview && (
        <div role="status" className="mt-3 space-y-2 text-sm" data-testid="edit-preview">
          {mine && mine.op !== "unchanged" ? (
            <ul className="list-disc pl-5">
              {mine.details.map((d) => (
                <li key={d}>{d}</li>
              ))}
            </ul>
          ) : (
            <p>Nothing changes.</p>
          )}
          {others.length > 0 && (
            <p>
              <Badge tone="warn">Also changes</Badge> {others.map((c) => `${c.kind} ${c.name}`).join(", ")}: the workspace plan differs from what runs.
            </p>
          )}
          {preview.problems.length > 0 && <ErrorText error={preview.problems.join("; ")} />}
          {!preview.fits && <p>{preview.fitNote}</p>}
          <Checkbox label="Restart the room now (otherwise it restarts the next time its agent is idle)" checked={now} onChange={(e) => setNow(e.target.checked)} />
        </div>
      )}
      <ErrorText error={plan.error ?? apply.error} />
      <div className="mt-4 flex justify-end gap-2">
        <Button onClick={onClose}>Cancel</Button>
        {preview && !preview.problems.length && mine && mine.op !== "unchanged" ? (
          <Button variant="primary" busy={apply.isPending} onClick={() => apply.mutate()}>
            Apply
          </Button>
        ) : (
          <Button variant="primary" disabled={!value} busy={plan.isPending} onClick={() => plan.mutate()}>
            Preview
          </Button>
        )}
      </div>
    </Dialog>
  );
}
