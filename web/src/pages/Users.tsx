import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { api } from "@/api";
import type { Role, User } from "@/api/types";
import { useMe } from "@/components/Shell";
import { Badge, Button, Card, ErrorText, PageHeader, Select, Spinner } from "@/components/ui";
import { ago } from "@/lib/format";

/** Admin: people's roles and workspace assignments (§8.1). */
export function UsersPage() {
  const me = useMe();
  const users = useQuery({ queryKey: ["users"], queryFn: () => api().listUsers(), enabled: me.data?.role === "admin" });
  const workspaces = useQuery({ queryKey: ["workspaces"], queryFn: () => api().listWorkspaces() });
  if (me.data?.role !== "admin") return <ErrorText error="Only admins can manage users." />;
  return (
    <div>
      <PageHeader title="Users" subtitle="People appear here after they first sign in. Identity-provider group mappings apply at each sign-in." />
      {users.isPending && <Spinner label="Loading" />}
      <ErrorText error={users.error} />
      <ul className="space-y-3">
        {users.data?.map((u) => (
          <li key={u.handle}>
            <UserRow u={u} workspaces={workspaces.data?.map((w) => w.name) ?? []} self={u.handle === me.data?.handle} />
          </li>
        ))}
      </ul>
    </div>
  );
}

function UserRow({ u, workspaces, self }: { u: User; workspaces: string[]; self: boolean }) {
  const qc = useQueryClient();
  const [role, setRole] = useState<Role>(u.role);
  const [ws, setWs] = useState<string[]>(u.workspaces);
  const dirty = role !== u.role || ws.join() !== u.workspaces.join();
  const save = useMutation({
    mutationFn: () => api().updateUser(u.handle, role, ws),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["users"] }),
  });
  return (
    <Card>
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <p className="font-semibold">
            {u.displayName} <span className="font-normal text-muted">({u.handle})</span>
          </p>
          <p className="text-sm text-muted">
            {u.email} · <Badge>{u.source}</Badge> · seen {ago(u.lastSeen)}
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Select aria-label={`Role of ${u.handle}`} value={role} disabled={self} onChange={(e) => setRole(e.target.value as Role)} className="w-auto">
            <option value="admin">admin</option>
            <option value="operator">operator</option>
            <option value="viewer">viewer</option>
          </Select>
          <Button variant="primary" disabled={!dirty} busy={save.isPending} onClick={() => save.mutate()}>
            Save
          </Button>
        </div>
      </div>
      {role !== "admin" && (
        <fieldset className="mt-2">
          <legend className="text-sm text-muted">Workspaces</legend>
          <div className="flex flex-wrap gap-x-4">
            {workspaces.map((w) => (
              <label key={w} className="inline-flex min-h-10 items-center gap-2 text-sm">
                <input type="checkbox" checked={ws.includes(w)} onChange={(e) => setWs(e.target.checked ? [...ws, w] : ws.filter((x) => x !== w))} />
                {w}
              </label>
            ))}
          </div>
        </fieldset>
      )}
      <ErrorText error={save.error} />
    </Card>
  );
}
