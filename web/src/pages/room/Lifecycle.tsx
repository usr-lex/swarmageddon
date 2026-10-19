import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { api } from "@/api";
import type { RoomAction, RoomSummary } from "@/api/types";
import { Button, Checkbox, Dialog, ErrorText, Field, Input } from "@/components/ui";
import { QuarantineAction } from "@/components/Warden";
import { EditRoom } from "./EditRoom";

type Pending = "recreate" | "remove" | "rotate" | null;

/** Pause, resume, recreate, remove, rotate and update-now (§5, lifecycle actions). */
export function Lifecycle({ room }: { room: RoomSummary }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [dialog, setDialog] = useState<Pending>(null);
  const [deleteData, setDeleteData] = useState(false);
  const [typed, setTyped] = useState("");
  const act = useMutation({
    mutationFn: (a: RoomAction) => api().roomAction(room.workspace, room.name, a),
    onSuccess: async (_, a) => {
      setDialog(null);
      await qc.invalidateQueries({ queryKey: ["room", room.workspace, room.name] });
      if (a.kind === "remove") await navigate({ to: "/fleet" });
    },
  });
  const open = (d: Pending) => {
    setDeleteData(false);
    setTyped("");
    act.reset();
    setDialog(d);
  };
  const nameOK = typed === room.name;

  return (
    <div className="flex flex-wrap gap-2" aria-label="Room actions" role="toolbar">
      {room.paused ? (
        <Button onClick={() => act.mutate({ kind: "resume" })} busy={act.isPending && act.variables?.kind === "resume"}>
          Resume
        </Button>
      ) : (
        <Button onClick={() => act.mutate({ kind: "pause" })} busy={act.isPending && act.variables?.kind === "pause"}>
          Pause
        </Button>
      )}
      {room.updateAvailable && (
        <Button onClick={() => act.mutate({ kind: "updateNow" })} busy={act.isPending && act.variables?.kind === "updateNow"}>
          Update now
        </Button>
      )}
      <EditRoom room={room} />
      <Button onClick={() => open("rotate")}>Rotate keys</Button>
      <Button onClick={() => open("recreate")}>Recreate</Button>
      <QuarantineAction room={room} />
      <Button variant="danger" onClick={() => open("remove")}>
        Remove
      </Button>
      {!dialog && <ErrorText error={act.error} />}

      <Dialog open={dialog === "rotate"} onOpenChange={(o) => !o && setDialog(null)} title={`Rotate ${room.name}'s keys?`}>
        <p className="text-sm">New SSH and GPG keys, Forgejo token, gateway key and messaging certificate are issued; the old ones are revoked. The room reloads them without a restart.</p>
        <ErrorText error={act.error} />
        <div className="mt-4 flex justify-end gap-2">
          <Button onClick={() => setDialog(null)}>Cancel</Button>
          <Button variant="primary" busy={act.isPending} onClick={() => act.mutate({ kind: "rotate" })}>
            Rotate
          </Button>
        </div>
      </Dialog>

      <Dialog
        open={dialog === "recreate"}
        onOpenChange={(o) => !o && setDialog(null)}
        title={`Recreate ${room.name}?`}
        description="The pod is deleted and started again. The engine volume and, unless you choose otherwise, the home volume are kept."
      >
        <Checkbox label="Also delete the home volume (the agent's files and sessions)" checked={deleteData} onChange={(e) => setDeleteData(e.target.checked)} />
        {deleteData && (
          <Field label={<>Type <code>{room.name}</code> to confirm</>}>
            {(id) => <Input id={id} value={typed} autoComplete="off" onChange={(e) => setTyped(e.target.value)} />}
          </Field>
        )}
        <ErrorText error={act.error} />
        <div className="mt-4 flex justify-end gap-2">
          <Button onClick={() => setDialog(null)}>Cancel</Button>
          <Button
            variant={deleteData ? "danger" : "primary"}
            disabled={deleteData && !nameOK}
            busy={act.isPending}
            onClick={() => act.mutate({ kind: "recreate", deleteHome: deleteData, confirmName: deleteData ? typed : undefined })}
          >
            {deleteData ? "Delete home and recreate" : "Recreate"}
          </Button>
        </div>
      </Dialog>

      <Dialog
        open={dialog === "remove"}
        onOpenChange={(o) => !o && setDialog(null)}
        title={`Remove ${room.name}?`}
        description="The room is deleted. Its Forgejo user is disabled, not deleted, so history keeps its author. Volumes are kept for the workspace's retention unless you delete them now."
      >
        <Checkbox label="Delete its data now" checked={deleteData} onChange={(e) => setDeleteData(e.target.checked)} />
        <Field label={<>Type <code>{room.name}</code> to confirm</>}>
          {(id) => <Input id={id} value={typed} autoComplete="off" onChange={(e) => setTyped(e.target.value)} />}
        </Field>
        <ErrorText error={act.error} />
        <div className="mt-4 flex justify-end gap-2">
          <Button onClick={() => setDialog(null)}>Cancel</Button>
          <Button variant="danger" disabled={!nameOK} busy={act.isPending} onClick={() => act.mutate({ kind: "remove", deleteDataNow: deleteData })}>
            Remove {room.name}
          </Button>
        </div>
      </Dialog>
    </div>
  );
}
