import { useMutation } from "@tanstack/react-query";
import { useState } from "react";
import { api } from "@/api";
import type { CheckResult, IsolationTier } from "@/api/types";
import { Badge, Button, Card, Checkbox, ErrorText, Spinner, type Tone } from "@/components/ui";
import type { StepProps } from "./PlatformWizard";

const resultTone: Record<CheckResult, Tone> = { pass: "ok", warn: "warn", fail: "bad", pending: "muted" };
const resultLabel: Record<CheckResult, string> = { pass: "Pass", warn: "Warning", fail: "Fail", pending: "Running" };

const tiers: { id: IsolationTier; label: string; text: string }[] = [
  { id: "auto", label: "Automatic", text: "Use the safest tier preflight found to work." },
  { id: "userns", label: "User namespaces", text: "Rooms run in their own user namespace. Recommended." },
  { id: "sysbox", label: "Sysbox", text: "Uses the sysbox-runc RuntimeClass." },
  { id: "privileged", label: "Privileged", text: "Rooms run as privileged pods." },
];

export function PreflightStep({ platform, save, saving }: StepProps) {
  const pf = platform.preflight;
  const [tier, setTier] = useState<IsolationTier>(platform.spec.isolationTier || "auto");
  const [accepted, setAccepted] = useState(platform.spec.privilegedAccepted);
  const rerun = useMutation({ mutationFn: () => api().rerunPreflight() });
  const failed = pf.checks.filter((c) => c.result === "fail");
  const needsAccept = tier === "privileged" || (tier === "auto" && pf.tier === "privileged");
  const canContinue = pf.completed && failed.length === 0 && (!needsAccept || accepted);

  return (
    <div className="space-y-4">
      <Card
        title="Cluster checks"
        actions={
          <Button onClick={() => rerun.mutate()} busy={rerun.isPending || !pf.completed}>
            Run again
          </Button>
        }
      >
        {!pf.completed && <Spinner label="Running checks" />}
        <ul className="divide-y divide-border" aria-label="Preflight checks">
          {pf.checks.map((c) => (
            <li key={c.name} className="flex flex-col gap-1 py-2 sm:flex-row sm:items-start sm:gap-3">
              <Badge tone={resultTone[c.result]} className="w-20 justify-center">
                {resultLabel[c.result]}
              </Badge>
              <div className="min-w-0 flex-1">
                <p className="text-sm font-medium">{c.name}</p>
                {c.message && <p className="text-sm text-muted">{c.message}</p>}
                {c.fix && c.result !== "pass" && (
                  <p className="mt-1 rounded bg-surface-2 p-2 text-sm">
                    <span className="font-medium">Fix: </span>
                    {c.fix}
                  </p>
                )}
              </div>
            </li>
          ))}
        </ul>
        <ErrorText error={rerun.error} />
      </Card>

      <Card title="Capacity">
        <p className="text-sm">
          About <strong data-testid="room-estimate">{pf.roomEstimate}</strong> rooms fit at the default size (2 CPU, 6 GiB memory, 90 GiB disk).
        </p>
        {!!pf.clusterCIDRs.length && <p className="mt-1 text-sm text-muted">Rooms will not reach the cluster ranges {pf.clusterCIDRs.join(", ")}.</p>}
      </Card>

      <Card title="Nested containers">
        <fieldset className="space-y-2">
          <legend className="mb-2 text-sm text-muted">
            How rooms run Podman. Preflight picked <strong>{pf.tier || "—"}</strong>.
          </legend>
          {tiers.map((t) => (
            <label key={t.id} className="flex min-h-10 cursor-pointer items-start gap-2 text-sm">
              <input type="radio" name="tier" value={t.id} checked={tier === t.id} onChange={() => setTier(t.id)} className="mt-1 accent-accent" />
              <span>
                <span className="font-medium">{t.label}</span> <span className="text-muted">{t.text}</span>
              </span>
            </label>
          ))}
        </fieldset>
        {needsAccept && (
          <div role="alert" className="mt-3 rounded-xs border border-bad/40 bg-bad/10 p-3 text-sm">
            <p className="font-semibold text-bad">Privileged rooms are not isolated from the node.</p>
            <p className="mt-1">
              An agent in a privileged room can take over the node and everything running on it, including other rooms and the platform's
              secrets. Use this only on a machine dedicated to swarmageddon.
            </p>
            <Checkbox
              className="mt-2"
              label="I understand that rooms will not be isolated, and accept the privileged tier."
              checked={accepted}
              onChange={(e) => setAccepted(e.target.checked)}
            />
          </div>
        )}
      </Card>

      {failed.length > 0 && <p className="text-sm text-bad">Fix the failed checks, then run them again.</p>}
      <div className="flex justify-end">
        <Button
          variant="primary"
          busy={saving}
          disabled={!canContinue}
          onClick={() => save({ ...platform.spec, isolationTier: tier, privilegedAccepted: needsAccept && accepted }, "signin")}
        >
          Continue
        </Button>
      </div>
    </div>
  );
}
