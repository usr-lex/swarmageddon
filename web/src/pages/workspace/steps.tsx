import { useMutation, useQuery } from "@tanstack/react-query";
import { api } from "@/api";
import { GitHubImport } from "./GitHubImport";
import { notificationEvents, type ChannelKind, type CLI, type ModelAccess, type RepositoryDraft, type RoomDraft } from "@/api/types";
import { Badge, Button, Card, Checkbox, Field, Input, Select } from "@/components/ui";
import { resizeRooms, roomNamePattern } from "@/lib/workspace";
import type { DraftProps } from "./WorkspaceWizard";

const dnsLabel = roomNamePattern.source.slice(1, -1);

export function BasicsStep({ draft, set, existing, models }: DraftProps & { models: string[] }) {
  return (
    <Card title="Workspace">
      <div className="grid gap-3 sm:grid-cols-2">
        <Field label="Name" hint="Lower case; the namespace is ws-<name>.">
          {(id, d) => (
            <Input
              id={id}
              aria-describedby={d}
              value={draft.name}
              required
              disabled={existing}
              pattern={dnsLabel}
              onChange={(e) => set({ ...draft, name: e.target.value, org: draft.org === draft.name ? e.target.value : draft.org })}
            />
          )}
        </Field>
        <Field label="Display name">{(id) => <Input id={id} value={draft.displayName} onChange={(e) => set({ ...draft, displayName: e.target.value })} />}</Field>
        <Field label="Forgejo organization">
          {(id) => <Input id={id} value={draft.org} required disabled={existing} pattern="[A-Za-z0-9][A-Za-z0-9_.-]*" onChange={(e) => set({ ...draft, org: e.target.value })} />}
        </Field>
        <Field label="Default model">
          {(id) => (
            <Select id={id} value={draft.defaultModel} onChange={(e) => set({ ...draft, defaultModel: e.target.value })}>
              {models.map((m) => (
                <option key={m}>{m}</option>
              ))}
            </Select>
          )}
        </Field>
        <Field
          label="Model access"
          hint={
            draft.modelAccess === "direct"
              ? "Each CLI uses its own provider: give rooms ANTHROPIC_API_KEY or OPENAI_API_KEY as a room secret, or log in once in a room's terminal. No spend limits, Spend page or gateway guard for these rooms."
              : "Through the platform's gateway: per-room keys, spend limits and the warden's gateway guard."
          }
        >
          {(id, d) => (
            <Select id={id} aria-describedby={d} value={draft.modelAccess ?? "gateway"} onChange={(e) => set({ ...draft, modelAccess: e.target.value as ModelAccess })}>
              <option value="gateway">Gateway (LiteLLM)</option>
              <option value="direct">Direct: each CLI's own key or login</option>
            </Select>
          )}
        </Field>
        <Field label="Monthly budget per agent (USD)" hint="0 is unlimited. 80% notifies; 100% stops model calls.">
          {(id, d) => (
            <Input id={id} aria-describedby={d} type="number" min={0} value={draft.perRoomMonthlyUSD} onChange={(e) => set({ ...draft, perRoomMonthlyUSD: Number(e.target.value) })} />
          )}
        </Field>
      </div>
      <div className="mt-3">
        <Checkbox
          label="Record what people type in terminals, in the audit log"
          checked={draft.recordKeystrokes ?? false}
          onChange={(e) => set({ ...draft, recordKeystrokes: e.target.checked })}
        />
      </div>
    </Card>
  );
}

export function AgentsStep({ draft, set, models }: DraftProps & { models: string[] }) {
  const templates = useQuery({ queryKey: ["roleTemplates"], queryFn: () => api().listRoleTemplates() });
  const setRoom = (i: number, patch: Partial<RoomDraft>) => set({ ...draft, rooms: draft.rooms.map((r, j) => (j === i ? { ...r, ...patch } : r)) });
  // Names come from the server's pool, skipping names used anywhere on the platform.
  const suggest = useMutation({ mutationFn: (n: number) => api().suggestAgentNames(n, draft.rooms.map((r) => r.name)) });
  const resize = async (n: number) => {
    n = Math.max(0, Math.min(50, n));
    const s = n > draft.rooms.length ? await suggest.mutateAsync(n) : { names: [], roles: [] };
    set({ ...draft, rooms: resizeRooms(draft.rooms, n, s, draft.defaultModel) });
  };
  const resetRoles = async () => {
    const { roles } = await suggest.mutateAsync(draft.rooms.length);
    set({ ...draft, rooms: draft.rooms.map((r, i) => ({ ...r, role: roles[i] ?? r.role })) });
  };
  const setAllCLI = (cli: CLI) => set({ ...draft, rooms: draft.rooms.map((r) => ({ ...r, cli })) });
  return (
    <Card
      title="Agents"
      actions={
        <div className="flex flex-wrap items-end gap-2">
          <Field label="How many">
            {(id) => <Input id={id} type="number" min={1} max={50} value={draft.rooms.length} onChange={(e) => resize(Number(e.target.value))} className="w-24" />}
          </Field>
          <Button onClick={resetRoles}>Default role mix</Button>
          <Button onClick={() => setAllCLI("claude")}>All Claude Code</Button>
          <Button onClick={() => setAllCLI("ore")}>All ore</Button>
        </div>
      }
    >
      {draft.rooms.length === 0 && <p className="text-sm text-muted">Choose how many agents this workspace has. Names come from a pool of Greek gods; change any of them.</p>}
      <ul className="space-y-3" aria-label="Agents">
        {draft.rooms.map((r, i) => {
          const clash = draft.rooms.findIndex((x) => x.name === r.name) !== i ? "Used twice." : undefined;
          return (
            <li key={i} className="grid gap-2 rounded-xs border border-border p-3 sm:grid-cols-6" data-testid="agent-row">
              <Field label="Name" error={clash}>
                {(id, d) => <Input id={id} aria-describedby={d} value={r.name} required pattern={dnsLabel} onChange={(e) => setRoom(i, { name: e.target.value })} />}
              </Field>
              <Field label="Role">
                {(id) => (
                  <Select id={id} value={r.role} onChange={(e) => setRoom(i, { role: e.target.value })}>
                    {(templates.data ?? [{ name: r.role, title: r.role, summary: "" }]).map((t) => (
                      <option key={t.name} value={t.name} title={t.summary}>
                        {t.title}
                      </option>
                    ))}
                  </Select>
                )}
              </Field>
              <Field label="CLI">
                {(id) => (
                  <Select id={id} value={r.cli} onChange={(e) => setRoom(i, { cli: e.target.value as CLI })}>
                    <option value="claude">Claude Code</option>
                    <option value="ore">ore</option>
                  </Select>
                )}
              </Field>
              <Field label="Model">
                {(id) => (
                  <Select id={id} value={r.model} onChange={(e) => setRoom(i, { model: e.target.value })}>
                    {[...new Set([r.model, ...models])].filter(Boolean).map((m) => (
                      <option key={m}>{m}</option>
                    ))}
                  </Select>
                )}
              </Field>
              <Field label="Effort">
                {(id) => (
                  <Select id={id} value={r.effort} onChange={(e) => setRoom(i, { effort: e.target.value })}>
                    {["low", "medium", "high", "xhigh", "max"].map((x) => (
                      <option key={x}>{x}</option>
                    ))}
                  </Select>
                )}
              </Field>
              <div className="flex items-end">
                <Button variant="ghost" aria-label={`Remove ${r.name}`} onClick={() => set({ ...draft, rooms: draft.rooms.filter((_, j) => j !== i) })}>
                  Remove
                </Button>
              </div>
            </li>
          );
        })}
      </ul>
    </Card>
  );
}

export function ResourcesStep({ draft, set }: DraftProps) {
  // The server's preview says whether the rooms fit (from preflight's capacity).
  const plan = useQuery({ queryKey: ["plan", draft], queryFn: () => api().planWorkspace(draft), placeholderData: (p) => p, enabled: draft.rooms.length > 0 });
  const setRoom = (i: number, patch: Partial<RoomDraft>) => set({ ...draft, rooms: draft.rooms.map((r, j) => (j === i ? { ...r, ...patch } : r)) });
  const setAll = (patch: Partial<RoomDraft>) => set({ ...draft, rooms: draft.rooms.map((r) => ({ ...r, ...patch })) });
  const num = (v: string) => Math.max(0, Number(v));
  const cols: [keyof RoomDraft, string, number][] = [
    ["cpu", "CPU", 0.5],
    ["memoryGi", "Memory (GiB)", 1],
    ["homeGi", "Home (GiB)", 5],
    ["engineGi", "Engine (GiB)", 5],
  ];
  return (
    <div className="space-y-4">
      {plan.data && (
        <div role="status" data-testid="fit" className={`border p-3 text-sm ${plan.data.fits ? "border-ok/40 bg-ok/10" : "border-alert/50 bg-alert/10"}`}>
          <Badge tone={plan.data.fits ? "ok" : "alert"}>{plan.data.fits ? "Fits" : "Doesn't fit"}</Badge> {plan.data.fitNote}
        </div>
      )}
      <Card
        title="Resources per agent"
        actions={
          <Button onClick={() => draft.rooms[0] && setAll({ cpu: draft.rooms[0].cpu, memoryGi: draft.rooms[0].memoryGi, homeGi: draft.rooms[0].homeGi, engineGi: draft.rooms[0].engineGi })}>
            Copy first to all
          </Button>
        }
      >
        <ul className="space-y-2">
          {draft.rooms.map((r, i) => (
            <li key={r.name + i} className="grid grid-cols-2 gap-2 sm:grid-cols-5 sm:items-end">
              <p className="col-span-2 self-center font-medium sm:col-span-1">{r.name}</p>
              {cols.map(([k, label, step]) => (
                <Field key={k} label={label}>
                  {(id) => <Input id={id} type="number" min={step} step={step} value={r[k] as number} onChange={(e) => setRoom(i, { [k]: num(e.target.value) })} />}
                </Field>
              ))}
            </li>
          ))}
        </ul>
      </Card>
    </div>
  );
}

const emptyRepo = (): RepositoryDraft => ({ name: "", importURL: "", importCredential: "", group: "", pushMirror: "", mirrorToken: "" });

export function ReposStep({ draft, set }: DraftProps) {
  const setRepo = (i: number, patch: Partial<RepositoryDraft>) => set({ ...draft, repositories: draft.repositories.map((r, j) => (j === i ? { ...r, ...patch } : r)) });
  return (
    <>
    <GitHubImport draft={draft} set={set} />
    <Card title="Repositories" actions={<Button onClick={() => set({ ...draft, repositories: [...draft.repositories, emptyRepo()] })}>Add repository</Button>}>
      {draft.repositories.length === 0 && <p className="text-sm text-muted">Optional. Agents can't create repositories themselves; add more later by editing the workspace.</p>}
      {draft.repositories.map((r, i) => (
        <fieldset key={i} className="mb-3 grid gap-3 rounded-xs border border-border p-3 sm:grid-cols-2">
          <legend className="px-1 text-sm font-medium">
            {draft.org || "org"}/{r.name || "…"}
          </legend>
          <Field label="Name">{(id) => <Input id={id} value={r.name} required pattern="[A-Za-z0-9_.-]+" onChange={(e) => setRepo(i, { name: e.target.value })} />}</Field>
          <Field label="Owned by group" hint="Empty: the default teams.">
            {(id, d) => (
              <Select id={id} aria-describedby={d} value={r.group} onChange={(e) => setRepo(i, { group: e.target.value })}>
                <option value="">(default)</option>
                {draft.groups.map((g) => (
                  <option key={g}>{g}</option>
                ))}
              </Select>
            )}
          </Field>
          <Field label="Import from URL" hint="Empty creates an empty repository.">
            {(id, d) => <Input id={id} aria-describedby={d} type="url" value={r.importURL} onChange={(e) => setRepo(i, { importURL: e.target.value })} />}
          </Field>
          {r.importURL && (
            <Field label="Import credential" hint="A token, or user:token. Only Forgejo uses it, once, for the import.">
              {(id, d) => <Input id={id} aria-describedby={d} type="password" autoComplete="off" value={r.importCredential} onChange={(e) => setRepo(i, { importCredential: e.target.value })} />}
            </Field>
          )}
          <Field label="Push mirror URL" hint="Optional, e.g. a GitHub repository.">
            {(id, d) => <Input id={id} aria-describedby={d} type="url" value={r.pushMirror} onChange={(e) => setRepo(i, { pushMirror: e.target.value })} />}
          </Field>
          {r.pushMirror && (
            <Field label="Mirror token" hint="A token, or user:token, stored in a Secret only Forgejo uses. Empty keeps the stored one.">
              {(id, d) => <Input id={id} aria-describedby={d} type="password" autoComplete="off" value={r.mirrorToken} onChange={(e) => setRepo(i, { mirrorToken: e.target.value })} />}
            </Field>
          )}
          <div className="sm:col-span-2">
            <Button variant="ghost" onClick={() => set({ ...draft, repositories: draft.repositories.filter((_, j) => j !== i) })}>
              Remove repository
            </Button>
          </div>
        </fieldset>
      ))}
    </Card>
    </>
  );
}

const channelLabels: Record<ChannelKind, string> = { webPush: "Browser push", ntfy: "ntfy", slack: "Slack", discord: "Discord", email: "Email" };
const channelTarget: Partial<Record<ChannelKind, string>> = { ntfy: "Topic", slack: "Webhook URL", discord: "Webhook URL", email: "SMTP URL (smtp://user:pass@host:587/?to=…)" };
const eventLabels: Record<string, string> = {
  approvalWaiting: "An approval is waiting",
  idleWithOpenWork: "An agent is idle with open work",
  ciRed: "CI failed on a default branch or PR",
  budget: "A budget reached 80%",
  diskLow: "Disk is running low",
  roomCrashed: "A room crashed",
  wardenFlag: "The warden flagged an agent",
};
export { eventLabels, channelLabels };

export function NotifyStep({ draft, set }: DraftProps) {
  const has = (k: ChannelKind) => draft.channels.some((c) => c.kind === k);
  const toggle = (k: ChannelKind, on: boolean) =>
    set({ ...draft, channels: on ? [...draft.channels, { kind: k, target: "" }] : draft.channels.filter((c) => c.kind !== k) });
  return (
    <div className="space-y-4">
      <Card title="Channels">
        {(Object.keys(channelLabels) as ChannelKind[]).map((k) => (
          <div key={k} className="flex flex-wrap items-end gap-3">
            <Checkbox label={channelLabels[k]} checked={has(k)} onChange={(e) => toggle(k, e.target.checked)} />
            {has(k) && channelTarget[k] && (
              <Field label={channelTarget[k]} className="min-w-60 flex-1">
                {(id) => (
                  <Input
                    id={id}
                    required
                    type={k === "ntfy" ? "text" : "url"}
                    value={draft.channels.find((c) => c.kind === k)?.target ?? ""}
                    onChange={(e) => set({ ...draft, channels: draft.channels.map((c) => (c.kind === k ? { ...c, target: e.target.value } : c)) })}
                  />
                )}
              </Field>
            )}
          </div>
        ))}
      </Card>
      <Card title="Events">
        {notificationEvents.map((ev) => (
          <Checkbox
            key={ev}
            label={eventLabels[ev]}
            checked={draft.events.includes(ev)}
            onChange={(e) => set({ ...draft, events: e.target.checked ? [...draft.events, ev] : draft.events.filter((x) => x !== ev) })}
          />
        ))}
        <p className="mt-2 text-xs text-muted">Each person picks their own channels per event in Notification settings.</p>
      </Card>
    </div>
  );
}
