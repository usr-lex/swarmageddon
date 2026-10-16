import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { api } from "@/api";
import type { Platform, PlatformSecrets, PlatformSpec, WizardStep } from "@/api/types";
import { ErrorText, Spinner, cx } from "@/components/ui";
import { useLivePlatform } from "@/lib/platform";
import { PreflightStep } from "./PreflightStep";
import { SignInStep } from "./SignInStep";
import { ServicesStep } from "./ServicesStep";
import { HostnameStep } from "./HostnameStep";

const steps: { id: WizardStep; label: string }[] = [
  { id: "preflight", label: "Preflight" },
  { id: "signin", label: "Sign-in" },
  { id: "services", label: "Platform services" },
  { id: "hostname", label: "Hostname & TLS" },
  { id: "workspace", label: "First workspace" },
];

export interface StepProps {
  platform: Platform;
  /** Saves the spec (and secrets), then moves the wizard to `next` if given. */
  save: (spec: PlatformSpec, next?: WizardStep, secrets?: PlatformSecrets) => Promise<void>;
  saving: boolean;
}

/** The first-run wizard (§3.4). It only edits the Platform resource, so it resumes where it was left. */
export function PlatformWizard() {
  const platform = useLivePlatform();
  const [step, setStep] = useState<WizardStep>();
  const qc = useQueryClient();
  const navigate = useNavigate();
  const saved = platform.data?.spec.wizardStep;
  useEffect(() => {
    if (!step && saved) setStep(saved === "done" ? "preflight" : saved);
  }, [saved, step]);

  const m = useMutation({
    mutationFn: async (v: { spec: PlatformSpec; next?: WizardStep; secrets?: PlatformSecrets }) =>
      api().updatePlatform(v.next ? { ...v.spec, wizardStep: furthest(v.spec.wizardStep, v.next) } : v.spec, v.secrets),
    onSuccess: async (p, v) => {
      qc.setQueryData(["platform"], p);
      if (v.next === "workspace") await navigate({ to: "/workspaces/new" });
      else if (v.next) setStep(v.next);
    },
  });

  if (!platform.data || !step) return <Spinner label="Loading platform" />;
  const reached = steps.findIndex((s) => s.id === saved);
  const props: StepProps = {
    platform: platform.data,
    save: async (spec, next, secrets) => {
      await m.mutateAsync({ spec, next, secrets });
    },
    saving: m.isPending,
  };
  return (
    <div className="space-y-4">
      <h1 className="text-2xl font-semibold">First-run setup</h1>
      <nav aria-label="Setup steps">
        <ol className="flex flex-wrap gap-2 text-sm">
          {steps.map((s, i) => {
            const enabled = saved === "done" || i <= reached;
            return (
              <li key={s.id}>
                <button
                  type="button"
                  disabled={!enabled || s.id === "workspace"}
                  onClick={() => setStep(s.id)}
                  aria-current={s.id === step ? "step" : undefined}
                  className={cx(
                    "min-h-10 rounded-full border px-3",
                    s.id === step ? "border-accent bg-accent text-accent-fg" : "border-border bg-surface",
                    !enabled && "opacity-50",
                  )}
                >
                  {i + 1}. {s.label}
                </button>
              </li>
            );
          })}
        </ol>
      </nav>
      <ErrorText error={m.error} />
      {step === "preflight" && <PreflightStep {...props} />}
      {step === "signin" && <SignInStep {...props} />}
      {step === "services" && <ServicesStep {...props} />}
      {step === "hostname" && <HostnameStep {...props} />}
    </div>
  );
}

/** The saved step only moves forward, so going back to edit doesn't lose progress. */
function furthest(a: WizardStep, b: WizardStep): WizardStep {
  const order: WizardStep[] = ["preflight", "signin", "services", "hostname", "workspace", "done"];
  return order.indexOf(a) > order.indexOf(b) ? a : b;
}
