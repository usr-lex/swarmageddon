import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { api } from "@/api";
import type { Platform, WorkspaceDraft } from "@/api/types";
import { Button, ErrorText, Spinner, cx } from "@/components/ui";
import { emptyDraft, validateDraft } from "@/lib/workspace";
import { AgentsStep, BasicsStep, NotifyStep, ReposStep, ResourcesStep } from "./steps";
import { GroupsStep } from "./GroupsStep";
import { ReviewStep } from "./ReviewStep";
import { ProvisioningProgress } from "./ProvisioningProgress";

const steps = ["Basics", "Agents", "Resources", "Repositories", "Groups & links", "Notifications", "Review"] as const;
type Step = (typeof steps)[number];

export interface DraftProps {
  draft: WorkspaceDraft;
  set: (d: WorkspaceDraft) => void;
  existing?: boolean;
}

/** The workspace wizard (§8.3): new workspaces and changes to existing ones. */
export function WorkspaceWizard({ existing }: { existing?: string }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const models = useQuery({ queryKey: ["models"], queryFn: () => api().listModels() });
  const loaded = useQuery({ queryKey: ["workspaceDraft", existing], queryFn: () => api().getWorkspaceDraft(existing!), enabled: !!existing });
  const platform = qc.getQueryData<Platform>(["platform"]);
  const firstRun = platform?.spec.wizardStep === "workspace";
  const [draft, setDraft] = useState<WorkspaceDraft>();
  const [step, setStep] = useState<Step>("Basics");
  const [applied, setApplied] = useState<string>();

  useEffect(() => {
    if (draft) return;
    if (existing && loaded.data) setDraft(loaded.data);
    else if (!existing && models.data) setDraft(emptyDraft(models.data[0] ?? ""));
  }, [draft, existing, loaded.data, models.data]);

  const skip = useMutation({
    mutationFn: async () => {
      const p = await api().getPlatform();
      return api().updatePlatform({ ...p.spec, wizardStep: "done" });
    },
    onSuccess: async (p) => {
      qc.setQueryData(["platform"], p);
      await navigate({ to: "/fleet" });
    },
  });

  if (applied) return <ProvisioningProgress workspace={applied} />;
  if (!draft) return <Spinner label="Loading" />;

  const i = steps.indexOf(step);
  const errors = validateDraft(draft);
  const props: DraftProps = { draft, set: setDraft, existing: !!existing };
  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h1 className="text-2xl font-semibold">{existing ? `Change workspace ${existing}` : firstRun ? "Your first workspace" : "New workspace"}</h1>
        {firstRun && (
          <Button variant="ghost" busy={skip.isPending} onClick={() => skip.mutate()}>
            Skip for now
          </Button>
        )}
      </div>
      <nav aria-label="Workspace steps">
        <ol className="flex flex-wrap gap-1 text-sm">
          {steps.map((s, j) => (
            <li key={s}>
              <button
                type="button"
                onClick={() => setStep(s)}
                aria-current={s === step ? "step" : undefined}
                className={cx("min-h-10 rounded-full border px-3", s === step ? "border-accent bg-accent text-accent-fg" : "border-border bg-surface")}
              >
                {j + 1}. {s}
              </button>
            </li>
          ))}
        </ol>
      </nav>
      <ErrorText error={skip.error} />
      <form
        onSubmit={(e) => {
          e.preventDefault();
          setStep(steps[Math.min(i + 1, steps.length - 1)]);
        }}
        className="space-y-4"
      >
        {step === "Basics" && <BasicsStep {...props} models={models.data ?? []} />}
        {step === "Agents" && <AgentsStep {...props} models={models.data ?? []} />}
        {step === "Resources" && <ResourcesStep {...props} />}
        {step === "Repositories" && <ReposStep {...props} />}
        {step === "Groups & links" && <GroupsStep {...props} />}
        {step === "Notifications" && <NotifyStep {...props} />}
        {step === "Review" && (
          <ReviewStep
            {...props}
            errors={errors}
            onApplied={async () => {
              setApplied(draft.name);
              if (firstRun) {
                const p = await api().getPlatform();
                if (p.spec.wizardStep === "workspace") await api().updatePlatform({ ...p.spec, wizardStep: "done" });
              }
              await qc.invalidateQueries({ queryKey: ["platform"] });
              await qc.invalidateQueries({ queryKey: ["me"] });
            }}
          />
        )}
        {step !== "Review" && (
          <div className="flex justify-between gap-2">
            <Button disabled={i === 0} onClick={() => setStep(steps[i - 1])}>
              Back
            </Button>
            <Button type="submit" variant="primary">
              Next
            </Button>
          </div>
        )}
      </form>
    </div>
  );
}
