import { useMutation, useQuery } from "@tanstack/react-query";
import { api } from "@/api";
import type { PlanChange } from "@/api/types";
import { Badge, Button, Card, ErrorText, Spinner, type Tone } from "@/components/ui";
import type { DraftProps } from "./WorkspaceWizard";

const opTone: Record<PlanChange["op"], Tone> = { create: "ok", update: "info", delete: "bad", unchanged: "muted" };

export function ReviewStep({ draft, errors, onApplied }: DraftProps & { errors: string[]; onApplied: () => Promise<void> }) {
  const plan = useQuery({
    queryKey: ["plan", draft],
    queryFn: () => api().planWorkspace(draft),
    enabled: errors.length === 0,
  });
  const apply = useMutation({ mutationFn: () => api().applyWorkspace(draft), onSuccess: onApplied });
  const changes = plan.data?.changes.filter((c) => c.op !== "unchanged") ?? [];
  const problems = [...errors, ...(plan.data?.problems ?? [])];
  return (
    <div className="space-y-4">
      {problems.length > 0 && (
        <div role="alert" className="rounded-xs border border-bad/40 bg-bad/10 p-3 text-sm">
          <p className="font-medium text-bad">Fix these first:</p>
          <ul className="list-disc pl-5">
            {problems.map((e) => (
              <li key={e}>{e}</li>
            ))}
          </ul>
        </div>
      )}
      <Card title="What will change">
        {plan.isFetching && <Spinner label="Computing changes" />}
        <ErrorText error={plan.error} />
        {plan.data && changes.length === 0 && <p className="text-sm text-muted">Nothing changes.</p>}
        <ul className="divide-y divide-border" aria-label="Planned changes">
          {plan.data?.changes.map((c) => (
            <li key={`${c.kind}/${c.name}`} className="flex flex-wrap items-start gap-2 py-2 text-sm">
              <Badge tone={opTone[c.op]} className="w-20 justify-center">
                {c.op}
              </Badge>
              <span className="font-medium">
                {c.kind} {c.name}
              </span>
              {c.details.length > 0 && (
                <ul className="w-full pl-22 font-mono text-xs text-muted">
                  {c.details.map((d) => (
                    <li key={d}>{d}</li>
                  ))}
                </ul>
              )}
            </li>
          ))}
        </ul>
      </Card>
      <ErrorText error={apply.error} />
      <div className="flex justify-end">
        <Button variant="primary" disabled={problems.length > 0 || !plan.data || changes.length === 0} busy={apply.isPending} onClick={() => apply.mutate()}>
          Apply
        </Button>
      </div>
    </div>
  );
}
