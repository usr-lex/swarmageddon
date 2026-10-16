import type { AdvisorDraft, WardenMode } from "@/api/channels";
import { Card, Checkbox, Field, Input, Select } from "@/components/ui";
import type { DraftProps } from "./WorkspaceWizard";

const noAdvisor = (): AdvisorDraft => ({ model: "", effort: "high", maxCallsPerHour: 10, native: false });
const efforts = ["low", "medium", "high", "xhigh", "max"];

const modeHelp: Record<WardenMode, string> = {
  off: "No warden. Only the gateway's budget limits apply.",
  monitor: "Rules in the room, trajectory reviews after each turn, and the gateway guard. Nothing is blocked unless a verdict is high.",
  gate: "Also judges each side-effecting call the rules didn't decide (about 1–5 s each). The judge can only deny or escalate.",
};

function AdvisorEditor({ a, onChange, models, label, claude }: { a: AdvisorDraft; onChange: (a: AdvisorDraft) => void; models: string[]; label: string; claude: boolean }) {
  return (
    <div className="grid gap-2 sm:grid-cols-4 sm:items-end">
      <Field label={`${label}: advisor model`}>
        {(id) => (
          <Select id={id} value={a.model} onChange={(e) => onChange({ ...a, model: e.target.value })}>
            <option value="">no advisor</option>
            {models.map((m) => (
              <option key={m}>{m}</option>
            ))}
          </Select>
        )}
      </Field>
      {a.model && (
        <>
          <Field label="Effort">
            {(id) => (
              <Select id={id} value={a.effort} onChange={(e) => onChange({ ...a, effort: e.target.value })}>
                {efforts.map((x) => (
                  <option key={x}>{x}</option>
                ))}
              </Select>
            )}
          </Field>
          <Field label="Max calls per hour">
            {(id) => <Input id={id} type="number" min={1} value={a.maxCallsPerHour} onChange={(e) => onChange({ ...a, maxCallsPerHour: Math.max(1, Number(e.target.value) || 1) })} />}
          </Field>
          {claude && (
            <div>
              <Checkbox label="Also Claude's native advisor" checked={a.native} onChange={(e) => onChange({ ...a, native: e.target.checked })} />
              {a.native && (
                <p role="alert" className="text-xs text-alert">
                  Unauditable: its advice comes back encrypted, so nobody can read or show it.
                </p>
              )}
            </div>
          )}
        </>
      )}
    </div>
  );
}

/** The warden (ADR 0011) and advisors (ADR 0010). */
export function WardenStep({ draft, set, models }: DraftProps & { models: string[] }) {
  const w = draft.warden;
  const setW = (patch: Partial<typeof w>) => set({ ...draft, warden: { ...w, ...patch } });
  const roles = [...new Set(draft.rooms.map((r) => r.role))];
  const roleUsesClaude = (role: string) => draft.rooms.some((r) => r.role === role && r.cli === "claude");
  return (
    <div className="space-y-4">
      <Card title="Warden">
        <p className="mb-3 text-sm text-muted">A small model that watches each agent for drift, overreach, loops and risky actions. Its verdicts show on the fleet and in each room's Activity tab.</p>
        <div className="grid gap-3 sm:grid-cols-3">
          <Field label="Mode" hint={modeHelp[w.mode]}>
            {(id, d) => (
              <Select id={id} aria-describedby={d} value={w.mode} onChange={(e) => setW({ mode: e.target.value as WardenMode })}>
                <option value="off">off</option>
                <option value="monitor">monitor</option>
                <option value="gate">gate</option>
              </Select>
            )}
          </Field>
          <Field label="Warden model" hint="A small, fast model on the gateway. None: rules only.">
            {(id, d) => (
              <Select id={id} aria-describedby={d} value={w.model} onChange={(e) => setW({ model: e.target.value })} disabled={w.mode === "off"}>
                <option value="">rules only</option>
                {models.map((m) => (
                  <option key={m}>{m}</option>
                ))}
              </Select>
            )}
          </Field>
          <Field label="Warden budget per room (USD/month)">
            {(id) => <Input id={id} type="number" min={0} value={w.monthlyUSD} onChange={(e) => setW({ monthlyUSD: Math.max(0, Number(e.target.value) || 0) })} />}
          </Field>
        </div>
        <Checkbox
          className="mt-2"
          label="Interrupt the agent's turn on a high-severity verdict (it then waits for a person)"
          checked={w.interruptOnHigh}
          disabled={w.mode === "off"}
          onChange={(e) => setW({ interruptOnHigh: e.target.checked })}
        />
        <Checkbox
          label="Quarantine on high verdicts from the gateway guard, not only critical ones"
          checked={w.quarantineOnHigh}
          disabled={w.mode === "off"}
          onChange={(e) => setW({ quarantineOnHigh: e.target.checked })}
        />
      </Card>

      <Card title="Advisors per role">
        <p className="mb-3 text-sm text-muted">
          An agent can ask its advisor (a stronger model, or higher effort) for a second opinion with <code>swarm advise</code>. The advisor reads, never writes; its answers stay in
          the room and show in the Activity tab.
        </p>
        <ul className="space-y-3">
          {roles.map((role) => (
            <li key={role} className="border border-border p-3" data-testid="advisor-role">
              <p className="label mb-2 !text-fg">{role}</p>
              <AdvisorEditor
                a={draft.advisors[role] ?? noAdvisor()}
                label={role}
                models={models}
                claude={roleUsesClaude(role)}
                onChange={(a) => set({ ...draft, advisors: { ...draft.advisors, [role]: a } })}
              />
            </li>
          ))}
        </ul>
      </Card>

      <Card title="Per-room overrides">
        <ul className="divide-y divide-border">
          {draft.rooms.map((r, i) => {
            const setRoom = (patch: Partial<typeof r>) => set({ ...draft, rooms: draft.rooms.map((x, j) => (j === i ? { ...x, ...patch } : x)) });
            return (
              <li key={r.name + i} className="space-y-2 py-2">
                <div className="flex flex-wrap items-end gap-3">
                  <p className="min-w-28 font-mono text-sm">{r.name}</p>
                  <Field label={`${r.name}: warden mode`}>
                    {(id) => (
                      <Select id={id} value={r.wardenMode ?? ""} onChange={(e) => setRoom({ wardenMode: e.target.value as WardenMode | "" })} className="w-auto">
                        <option value="">workspace default ({w.mode})</option>
                        <option value="off">off</option>
                        <option value="monitor">monitor</option>
                        <option value="gate">gate</option>
                      </Select>
                    )}
                  </Field>
                  <Checkbox
                    label="Own advisor settings"
                    checked={r.advisor !== undefined}
                    onChange={(e) => setRoom({ advisor: e.target.checked ? { ...(draft.advisors[r.role] ?? noAdvisor()) } : undefined })}
                  />
                </div>
                {r.advisor !== undefined && (
                  <AdvisorEditor a={r.advisor ?? noAdvisor()} label={r.name} models={models} claude={r.cli === "claude"} onChange={(a) => setRoom({ advisor: a.model ? a : null })} />
                )}
              </li>
            );
          })}
        </ul>
      </Card>
    </div>
  );
}
