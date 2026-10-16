import { useState } from "react";
import type { GroupLink, LinkMode } from "@/api/types";
import { Button, Card, Field, Input, Select } from "@/components/ui";
import { groupsInUse, roomNamePattern } from "@/lib/workspace";
import { ReachDiagram } from "./ReachDiagram";
import type { DraftProps } from "./WorkspaceWizard";

const newLink = (from: string, to: string): GroupLink => ({ from, to, mode: "mediated", senders: [], recipients: [], requirePrefix: "" });

export function GroupsStep({ draft, set }: DraftProps) {
  const [name, setName] = useState("");
  const groups = groupsInUse(draft);
  const addGroup = () => {
    if (!roomNamePattern.test(name) || draft.groups.includes(name)) return;
    set({ ...draft, groups: [...draft.groups, name] });
    setName("");
  };
  const removeGroup = (g: string) =>
    set({
      ...draft,
      groups: draft.groups.filter((x) => x !== g),
      rooms: draft.rooms.map((r) => (r.group === g ? { ...r, group: "" } : r)),
      links: draft.links.filter((l) => l.from !== g && l.to !== g),
    });
  const setLink = (i: number, patch: Partial<GroupLink>) => set({ ...draft, links: draft.links.map((l, j) => (j === i ? { ...l, ...patch } : l)) });
  const inGroup = (g: string) => draft.rooms.filter((r) => r.group === g).map((r) => r.name);
  const toggle = (list: string[], v: string, on: boolean) => (on ? [...list, v] : list.filter((x) => x !== v));

  return (
    <div className="space-y-4">
      <Card title="Groups">
        <p className="mb-2 text-sm text-muted">
          Groups split the team, for example a clean side and a dirty side. Each group gets its own Forgejo team and messaging rules.
        </p>
        <div className="flex flex-wrap items-end gap-2">
          <Field label="New group">
            {(id) => (
              <Input
                id={id}
                value={name}
                onChange={(e) => setName(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") {
                    e.preventDefault();
                    addGroup();
                  }
                }}
              />
            )}
          </Field>
          <Button onClick={addGroup} disabled={!roomNamePattern.test(name) || draft.groups.includes(name)}>
            Add group
          </Button>
        </div>
        {draft.groups.length > 0 && (
          <ul className="mt-3 flex flex-wrap gap-2">
            {draft.groups.map((g) => (
              <li key={g} className="flex items-center gap-1 rounded-full border border-border px-3 py-1 text-sm">
                {g}
                <button type="button" className="min-h-6 px-1 text-muted hover:text-bad" aria-label={`Remove group ${g}`} onClick={() => removeGroup(g)}>
                  ×
                </button>
              </li>
            ))}
          </ul>
        )}
        {draft.groups.length > 0 && (
          <div className="mt-4 grid gap-2 sm:grid-cols-3">
            {draft.rooms.map((r, i) => (
              <Field key={r.name + i} label={`${r.name}'s group`}>
                {(id) => (
                  <Select id={id} value={r.group} onChange={(e) => set({ ...draft, rooms: draft.rooms.map((x, j) => (j === i ? { ...x, group: e.target.value } : x)) })}>
                    <option value="">(no group)</option>
                    {draft.groups.map((g) => (
                      <option key={g}>{g}</option>
                    ))}
                  </Select>
                )}
              </Field>
            ))}
          </div>
        )}
      </Card>

      <Card
        title="Who can message whom"
        actions={
          groups.length > 1 && <Button onClick={() => set({ ...draft, links: [...draft.links, newLink(groups[0], groups[1])] })}>Add link</Button>
        }
      >
        <Field label="Within a group" className="mb-3 max-w-xs">
          {(id) => (
            <Select id={id} value={draft.defaultLinks} onChange={(e) => set({ ...draft, defaultLinks: e.target.value as typeof draft.defaultLinks })}>
              <option value="withinGroup">agents reach each other</option>
              <option value="none">agents can't message each other</option>
            </Select>
          )}
        </Field>
        {draft.links.map((l, i) => (
          <fieldset key={i} className="mb-3 grid gap-3 rounded-md border border-border p-3 sm:grid-cols-3">
            <legend className="px-1 text-sm font-medium">Link {i + 1}</legend>
            <Field label="From">
              {(id) => (
                <Select id={id} value={l.from} onChange={(e) => setLink(i, { from: e.target.value, senders: [] })}>
                  {groups.map((g) => (
                    <option key={g} value={g}>
                      {g || "(no group)"}
                    </option>
                  ))}
                </Select>
              )}
            </Field>
            <Field label="To">
              {(id) => (
                <Select id={id} value={l.to} onChange={(e) => setLink(i, { to: e.target.value, recipients: [] })}>
                  {groups.map((g) => (
                    <option key={g} value={g}>
                      {g || "(no group)"}
                    </option>
                  ))}
                </Select>
              )}
            </Field>
            <Field label="Mode">
              {(id) => (
                <Select id={id} value={l.mode} onChange={(e) => setLink(i, { mode: e.target.value as LinkMode })}>
                  <option value="open">open: anyone to anyone</option>
                  <option value="mediated">mediated: named senders and recipients</option>
                  <option value="none">none</option>
                </Select>
              )}
            </Field>
            {l.mode === "mediated" && (
              <>
                <fieldset className="text-sm">
                  <legend className="font-medium">Senders (empty: anyone)</legend>
                  {inGroup(l.from).map((n) => (
                    <label key={n} className="mr-3 inline-flex min-h-8 items-center gap-1">
                      <input type="checkbox" checked={l.senders.includes(n)} onChange={(e) => setLink(i, { senders: toggle(l.senders, n, e.target.checked) })} />
                      {n}
                    </label>
                  ))}
                </fieldset>
                <fieldset className="text-sm">
                  <legend className="font-medium">Recipients (empty: anyone)</legend>
                  {inGroup(l.to).map((n) => (
                    <label key={n} className="mr-3 inline-flex min-h-8 items-center gap-1">
                      <input type="checkbox" checked={l.recipients.includes(n)} onChange={(e) => setLink(i, { recipients: toggle(l.recipients, n, e.target.checked) })} />
                      {n}
                    </label>
                  ))}
                </fieldset>
                <Field label="Required prefix" hint="{recipient} becomes the recipient's name.">
                  {(id, d) => <Input id={id} aria-describedby={d} value={l.requirePrefix} placeholder="to {recipient}:" onChange={(e) => setLink(i, { requirePrefix: e.target.value })} />}
                </Field>
              </>
            )}
            <div className="sm:col-span-3">
              <Button variant="ghost" onClick={() => set({ ...draft, links: draft.links.filter((_, j) => j !== i) })}>
                Remove link
              </Button>
            </div>
          </fieldset>
        ))}
        <ReachDiagram draft={draft} />
      </Card>
    </div>
  );
}
