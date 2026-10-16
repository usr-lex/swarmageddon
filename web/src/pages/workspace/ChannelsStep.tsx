import { useEffect } from "react";
import { api } from "@/api";
import type { WorkspaceDraft } from "@/api/types";
import { deliveryModes, selectorKinds, type ChannelAccess, type ChannelMemberDraft, type ChatChannelDraft, type DeliveryMode } from "@/api/channels";
import { Button, Card, Field, Input, Select } from "@/components/ui";
import { defaultChatChannels, wallProblems } from "@/lib/workspace";
import type { DraftProps } from "./WorkspaceWizard";

const newChannel = (): ChatChannelDraft => ({
  name: "",
  description: "",
  members: [{ select: "everyone", access: "readWrite" }],
  defaultDelivery: "digest",
  allowedDelivery: [],
  postsPerMinute: 20,
});

const accessLabel: Record<ChannelAccess, string> = { read: "read", write: "write (post only)", readWrite: "read and write" };

/** One selector, split into its kind and value for editing. */
function SelectorEditor({ m, onChange, values, label }: { m: ChannelMemberDraft; onChange: (m: ChannelMemberDraft) => void; values: Record<string, string[]>; label: string }) {
  const [kind, value = ""] = m.select.split(":");
  const needsValue = kind !== "everyone" && kind !== "people";
  const options = values[kind] ?? [];
  return (
    <div className="flex flex-wrap items-end gap-2">
      <Field label={`${label}: who`}>
        {(id) => (
          <Select id={id} value={kind} onChange={(e) => onChange({ ...m, select: e.target.value === "everyone" || e.target.value === "people" ? e.target.value : `${e.target.value}:${values[e.target.value]?.[0] ?? ""}` })} className="w-auto">
            {selectorKinds.map((k) => (
              <option key={k} value={k}>
                {k}
              </option>
            ))}
          </Select>
        )}
      </Field>
      {needsValue && (
        <Field label={`${label}: ${kind}`}>
          {(id) =>
            options.length ? (
              <Select id={id} value={value} onChange={(e) => onChange({ ...m, select: `${kind}:${e.target.value}` })} className="w-auto">
                {options.map((o) => (
                  <option key={o}>{o}</option>
                ))}
              </Select>
            ) : (
              <Input id={id} value={value} pattern="[a-z0-9][a-z0-9_-]*" onChange={(e) => onChange({ ...m, select: `${kind}:${e.target.value}` })} className="w-40" />
            )
          }
        </Field>
      )}
      <Field label={`${label}: access`}>
        {(id) => (
          <Select id={id} value={m.access} onChange={(e) => onChange({ ...m, access: e.target.value as ChannelAccess })} className="w-auto">
            {(Object.keys(accessLabel) as ChannelAccess[]).map((a) => (
              <option key={a} value={a}>
                {accessLabel[a]}
              </option>
            ))}
          </Select>
        )}
      </Field>
    </div>
  );
}

/** The server's default channels for the draft (DefaultChannels); the local copy if that fails. */
async function defaults(d: WorkspaceDraft): Promise<ChatChannelDraft[]> {
  try {
    return await api().defaultChannels(d);
  } catch {
    return defaultChatChannels(d);
  }
}

/** Channels and storm control (§7.8). */
export function ChannelsStep({ draft, set, existing }: DraftProps) {
  // A new workspace starts from the default channels.
  useEffect(() => {
    if (!existing && draft.chatChannels.length === 0 && draft.rooms.length > 0) void defaults(draft).then((chatChannels) => set({ ...draft, chatChannels }));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  const values: Record<string, string[]> = {
    role: [...new Set(draft.rooms.map((r) => r.role))],
    group: draft.groups,
    room: draft.rooms.map((r) => r.name),
  };
  const setCh = (i: number, patch: Partial<ChatChannelDraft>) => set({ ...draft, chatChannels: draft.chatChannels.map((c, j) => (j === i ? { ...c, ...patch } : c)) });
  const walls = wallProblems(draft);
  const st = draft.storm;
  const setStorm = (patch: Partial<typeof st>) => set({ ...draft, storm: { ...st, ...patch } });
  const num = (v: string) => Math.max(0, Number(v) || 0);

  return (
    <div className="space-y-4">
      <Card
        title="Channels"
        actions={
          <div className="flex gap-2">
            <Button onClick={() => void defaults(draft).then((chatChannels) => set({ ...draft, chatChannels }))}>Reset to defaults</Button>
            <Button onClick={() => set({ ...draft, chatChannels: [...draft.chatChannels, newChannel()] })}>Add channel</Button>
          </div>
        }
      >
        <p className="mb-3 text-sm text-muted">
          One-to-many conversations. Agents mostly pull them; an @mention always pushes to that agent. Members come from selectors, and a member gets the highest access any
          entry gives it.{draft.groups.length > 1 && " In a walled workspace, a channel's members must come from groups that open links join both ways."}
        </p>
        {walls.length > 0 && (
          <ul role="alert" className="mb-3 list-disc border border-alert/50 bg-alert/10 p-3 pl-6 text-sm">
            {walls.map((w) => (
              <li key={w}>{w}</li>
            ))}
          </ul>
        )}
        <ul className="space-y-3" aria-label="Channels">
          {draft.chatChannels.map((c, i) => (
            <li key={i} className="border border-border p-3" data-testid="channel-row">
              <div className="grid gap-3 sm:grid-cols-3">
                <Field label="Name">
                  {(id) => (
                    <div className="flex items-center">
                      <span className="pr-1 text-muted">#</span>
                      <Input id={id} value={c.name} required pattern="[a-z0-9][a-z0-9_\-]{0,62}" onChange={(e) => setCh(i, { name: e.target.value })} />
                    </div>
                  )}
                </Field>
                <Field label="Description" className="sm:col-span-2">
                  {(id) => <Input id={id} value={c.description} onChange={(e) => setCh(i, { description: e.target.value })} />}
                </Field>
              </div>
              <fieldset className="mt-3 space-y-2">
                <legend className="label">Members</legend>
                {c.members.map((m, k) => (
                  <div key={k} className="flex flex-wrap items-end gap-2">
                    <SelectorEditor m={m} label={`#${c.name || "channel"} member ${k + 1}`} values={values} onChange={(nm) => setCh(i, { members: c.members.map((x, j) => (j === k ? nm : x)) })} />
                    <Button variant="ghost" aria-label={`Remove member ${k + 1} of #${c.name}`} onClick={() => setCh(i, { members: c.members.filter((_, j) => j !== k) })}>
                      Remove
                    </Button>
                  </div>
                ))}
                <Button onClick={() => setCh(i, { members: [...c.members, { select: "people", access: "readWrite" }] })}>Add member</Button>
              </fieldset>
              <div className="mt-3 grid gap-3 sm:grid-cols-3">
                <Field label="Default delivery">
                  {(id) => (
                    <Select id={id} value={c.defaultDelivery} onChange={(e) => setCh(i, { defaultDelivery: e.target.value as DeliveryMode })}>
                      {deliveryModes.map((d) => (
                        <option key={d}>{d}</option>
                      ))}
                    </Select>
                  )}
                </Field>
                <fieldset>
                  <legend className="mb-1 text-sm font-medium">Readers may switch to</legend>
                  <div className="flex flex-wrap gap-3">
                    {deliveryModes.map((d) => (
                      <label key={d} className="inline-flex min-h-8 items-center gap-1 text-sm">
                        <input
                          type="checkbox"
                          checked={c.allowedDelivery.length === 0 || c.allowedDelivery.includes(d)}
                          onChange={(e) => {
                            const cur = c.allowedDelivery.length ? c.allowedDelivery : deliveryModes;
                            const next = e.target.checked ? [...new Set([...cur, d])] : cur.filter((x) => x !== d);
                            setCh(i, { allowedDelivery: next.length === deliveryModes.length ? [] : next });
                          }}
                        />
                        {d}
                      </label>
                    ))}
                  </div>
                </fieldset>
                <Field label="Posts per minute">
                  {(id) => <Input id={id} type="number" min={1} value={c.postsPerMinute} onChange={(e) => setCh(i, { postsPerMinute: num(e.target.value) })} />}
                </Field>
              </div>
              <Button className="mt-2" variant="ghost" onClick={() => set({ ...draft, chatChannels: draft.chatChannels.filter((_, j) => j !== i) })}>
                Remove #{c.name || "channel"}
              </Button>
            </li>
          ))}
        </ul>
      </Card>
      <Card title="Storm control">
        <p className="mb-3 text-sm text-muted">Server-side limits that keep an agent's session from being flooded. Approval answers, warden notices and people's direct messages are never held back.</p>
        <div className="grid gap-3 sm:grid-cols-4">
          <Field label="Pushes per minute per agent" hint="Beyond this, items wait and the agent gets one notice.">
            {(id, d) => <Input id={id} aria-describedby={d} type="number" min={1} value={st.pushPerMinute} onChange={(e) => setStorm({ pushPerMinute: num(e.target.value) })} />}
          </Field>
          <Field label="Coalesce up to (bytes)" hint="Waiting items arrive as one message of at most this size.">
            {(id, d) => <Input id={id} aria-describedby={d} type="number" min={512} step={512} value={st.coalesceBytes} onChange={(e) => setStorm({ coalesceBytes: num(e.target.value) })} />}
          </Field>
          <Field label="Digest interval (minutes)">
            {(id) => <Input id={id} type="number" min={1} value={st.digestMinutes} onChange={(e) => setStorm({ digestMinutes: num(e.target.value) })} />}
          </Field>
          <Field label="Fan-out per minute per sender" hint="Recipients a sender may reach per minute.">
            {(id, d) => <Input id={id} aria-describedby={d} type="number" min={1} value={st.fanoutPerMinute} onChange={(e) => setStorm({ fanoutPerMinute: num(e.target.value) })} />}
          </Field>
        </div>
      </Card>
    </div>
  );
}
