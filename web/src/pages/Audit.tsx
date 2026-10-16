import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { api } from "@/api";
import type { AuditFilter } from "@/api/types";
import { Button, Empty, ErrorText, Field, Input, PageHeader, Spinner } from "@/components/ui";
import { downloadText, toJSONL } from "@/lib/download";
import { time } from "@/lib/format";

export function AuditPage() {
  const [form, setForm] = useState<AuditFilter>({ limit: 500 });
  const [filter, setFilter] = useState<AuditFilter>(form);
  const q = useQuery({ queryKey: ["audit", filter], queryFn: () => api().queryAudit(filter) });
  const set = (k: keyof AuditFilter, v: string) => setForm({ ...form, [k]: v || undefined });
  // datetime-local has no zone; convert to ISO in the browser's zone.
  const iso = (v: string) => (v ? new Date(v).toISOString() : "");
  return (
    <div>
      <PageHeader
        title="Audit log"
        actions={
          <Button disabled={!q.data?.length} onClick={() => downloadText(`audit-${new Date().toISOString().slice(0, 10)}.jsonl`, toJSONL(q.data ?? []))}>
            Export JSONL
          </Button>
        }
      />
      <form
        className="mb-4 grid gap-2 sm:grid-cols-3 lg:grid-cols-6 lg:items-end"
        onSubmit={(e) => {
          e.preventDefault();
          setFilter(form);
        }}
      >
        <Field label="Who">{(id) => <Input id={id} value={form.actor ?? ""} onChange={(e) => set("actor", e.target.value)} />}</Field>
        <Field label="Action">{(id) => <Input id={id} placeholder="e.g. room." value={form.action ?? ""} onChange={(e) => set("action", e.target.value)} />}</Field>
        <Field label="Workspace">{(id) => <Input id={id} value={form.workspace ?? ""} onChange={(e) => set("workspace", e.target.value)} />}</Field>
        <Field label="Since">{(id) => <Input id={id} type="datetime-local" onChange={(e) => set("since", iso(e.target.value))} />}</Field>
        <Field label="Until">{(id) => <Input id={id} type="datetime-local" onChange={(e) => set("until", iso(e.target.value))} />}</Field>
        <Button type="submit" variant="primary">
          Filter
        </Button>
      </form>
      {q.isPending && <Spinner label="Loading" />}
      <ErrorText error={q.error} />
      {q.data?.length === 0 && <Empty>No entries match.</Empty>}
      {!!q.data?.length && (
        <div className="overflow-x-auto rounded-lg border border-border bg-surface">
          <table className="w-full text-left text-sm">
            <caption className="sr-only">Audit entries</caption>
            <thead className="bg-surface-2 text-xs text-muted">
              <tr>
                {["When", "Who", "Action", "Object", "Detail", "From"].map((h) => (
                  <th key={h} scope="col" className="px-3 py-2 font-medium">
                    {h}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody className="divide-y divide-border">
              {q.data.map((e) => (
                <tr key={e.id}>
                  <td className="px-3 py-2 whitespace-nowrap">{time(e.at)}</td>
                  <td className="px-3 py-2">{e.actor}</td>
                  <td className="px-3 py-2 font-mono text-xs">{e.action}</td>
                  <td className="px-3 py-2">{e.object}</td>
                  <td className="max-w-md px-3 py-2 break-words">{e.detail}</td>
                  <td className="px-3 py-2 text-muted">{e.sourceIP}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
