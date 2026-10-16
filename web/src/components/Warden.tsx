import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { api } from "@/api";
import type { Severity, Verdict } from "@/api/channels";
import type { RoomSummary } from "@/api/types";
import { ago } from "@/lib/format";
import { Badge, Button, Dialog, ErrorText, Field, Spinner, Textarea, cx, type Tone } from "./ui";

/** Severity colors: the warm accent only for high and critical. */
export const severityTone: Record<Severity, Tone> = { low: "muted", medium: "warn", high: "alert", critical: "alert" };

export function VerdictBadge({ severity, className }: { severity: Severity; className?: string }) {
  return (
    <Badge tone={severityTone[severity]} className={cx(severity === "critical" && "font-bold", className)}>
      <span aria-hidden>{severity === "critical" ? "◆" : severity === "high" ? "▲" : severity === "medium" ? "■" : "·"}</span>
      warden {severity}
    </Badge>
  );
}

export function QuarantineBadge() {
  return (
    <Badge tone="alert">
      <span aria-hidden>⊘</span> quarantined
    </Badge>
  );
}

/** The quarantine banner on a room page, with Release for operators. */
export function QuarantineBanner({ room, canOperate }: { room: RoomSummary; canOperate: boolean }) {
  const qc = useQueryClient();
  const release = useMutation({
    mutationFn: () => api().roomAction(room.workspace, room.name, { kind: "release" }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["room", room.workspace, room.name] }),
  });
  if (!room.quarantined) return null;
  return (
    <div role="alert" className="flex flex-wrap items-center justify-between gap-3 border border-alert bg-alert/10 p-3" data-testid="quarantine-banner">
      <div>
        <p className="label !text-alert">Quarantined</p>
        <p className="text-sm">
          {room.quarantineReason || "The warden quarantined this room."} Its model requests are refused and only the gateway is reachable until a person releases it.
        </p>
      </div>
      {canOperate && (
        <Button variant="danger" busy={release.isPending} onClick={() => release.mutate()}>
          Release
        </Button>
      )}
      <ErrorText error={release.error} />
    </div>
  );
}

/** "Quarantine now": asks for a reason, then cuts the room off. */
export function QuarantineAction({ room }: { room: RoomSummary }) {
  const qc = useQueryClient();
  const [open, setOpen] = useState(false);
  const [reason, setReason] = useState("");
  const q = useMutation({
    mutationFn: () => api().roomAction(room.workspace, room.name, { kind: "quarantine", reason }),
    onSuccess: async () => {
      setOpen(false);
      await qc.invalidateQueries({ queryKey: ["room", room.workspace, room.name] });
    },
  });
  if (room.quarantined) return null;
  return (
    <>
      <Button variant="danger" onClick={() => setOpen(true)}>
        Quarantine now
      </Button>
      <Dialog
        open={open}
        onOpenChange={setOpen}
        title={`Quarantine ${room.name}?`}
        description="Its model requests are refused, its network is cut to the gateway only, and its forge token is paused, until someone releases it."
      >
        <Field label="Reason (shown on the room and in the audit log)">
          {(id) => <Textarea id={id} rows={3} value={reason} onChange={(e) => setReason(e.target.value)} className="font-sans" />}
        </Field>
        <ErrorText error={q.error} />
        <div className="mt-4 flex justify-end gap-2">
          <Button onClick={() => setOpen(false)}>Cancel</Button>
          <Button variant="danger" busy={q.isPending} disabled={!reason.trim()} onClick={() => q.mutate()}>
            Quarantine
          </Button>
        </div>
      </Dialog>
    </>
  );
}

const layerLabel: Record<Verdict["layer"], string> = { rules: "Rules", judge: "Judge", trajectory: "Trajectory", gateway: "Gateway guard" };

/** The room's warden verdicts, newest first, with confirm/dismiss for operators (ADR 0011). */
export function WardenPanel({ workspace, room, canOperate }: { workspace: string; room: string; canOperate: boolean }) {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["verdicts", workspace, room], queryFn: () => api().listVerdicts(workspace, room) });
  const decide = useMutation({
    mutationFn: (v: { id: string; d: "confirmed" | "dismissed" }) => api().decideVerdict(v.id, v.d),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["verdicts", workspace, room] }),
  });
  if (q.isPending) return <Spinner label="Loading verdicts" />;
  if (q.error) return <ErrorText error={q.error} />;
  if (!q.data.length) return <p className="text-sm text-muted">No verdicts. The warden hasn't flagged anything.</p>;
  return (
    <ol className="space-y-2" aria-label="Warden verdicts">
      {q.data.map((v) => (
        <li key={v.id} className={cx("border-l-2 py-1 pl-3", v.decision?.kind === "dismissed" ? "border-border opacity-60" : v.severity === "high" || v.severity === "critical" ? "border-alert" : "border-warn")} data-testid="verdict">
          <div className="flex flex-wrap items-center gap-2">
            <VerdictBadge severity={v.severity} />
            <span className="label">
              {layerLabel[v.layer]} · {v.source === "gateway" ? "from the gateway" : "self-reported by the room"} · {v.action} · {ago(v.at)}
            </span>
          </div>
          <p className="mt-1 text-sm">
            <span className="font-mono text-xs text-muted">{v.rule}</span> {v.reason}
          </p>
          <pre className="mt-1 max-h-32 overflow-auto bg-surface-2 p-2 text-xs whitespace-pre-wrap break-all">{v.evidence}</pre>
          {v.decision ? (
            <p className="label mt-1">
              {v.decision.kind} by {v.decision.by} · {ago(v.decision.at)}
            </p>
          ) : (
            canOperate && (
              <div className="mt-1 flex gap-2">
                <Button className="!min-h-8" busy={decide.isPending && decide.variables?.id === v.id && decide.variables.d === "confirmed"} onClick={() => decide.mutate({ id: v.id, d: "confirmed" })}>
                  Confirm
                </Button>
                <Button className="!min-h-8" variant="ghost" busy={decide.isPending && decide.variables?.id === v.id && decide.variables.d === "dismissed"} onClick={() => decide.mutate({ id: v.id, d: "dismissed" })}>
                  Dismiss
                </Button>
              </div>
            )
          )}
        </li>
      ))}
      <ErrorText error={decide.error} />
    </ol>
  );
}
