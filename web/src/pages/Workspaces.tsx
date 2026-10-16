import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { api } from "@/api";
import { useMe } from "@/components/Shell";
import { Badge, Card, Empty, ErrorText, PageHeader, Spinner } from "@/components/ui";

export function WorkspacesPage() {
  const me = useMe();
  const q = useQuery({ queryKey: ["workspaces"], queryFn: () => api().listWorkspaces() });
  const canCreate = me.data?.role === "admin" || me.data?.role === "operator";
  return (
    <div>
      <PageHeader
        title="Workspaces"
        actions={
          canCreate && (
            <Link to="/workspaces/new" className="inline-flex min-h-10 items-center rounded-xs bg-accent px-3 text-sm font-medium text-accent-fg">
              New workspace
            </Link>
          )
        }
      />
      {q.isPending && <Spinner label="Loading" />}
      <ErrorText error={q.error} />
      {q.data?.length === 0 && <Empty>No workspaces yet.</Empty>}
      <ul className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
        {q.data?.map((w) => (
          <li key={w.name}>
            <Card title={w.displayName || w.name} actions={<Badge tone={w.ready ? "ok" : "info"}>{w.ready ? "Ready" : "Provisioning"}</Badge>}>
              <p className="text-sm text-muted">
                {w.name} · {w.rooms} rooms
              </p>
              <Link to="/w/$workspace" params={{ workspace: w.name }} className="mt-2 mr-3 inline-block text-sm underline">
                Observatory
              </Link>
              {canCreate && (
                <Link to="/workspaces/$workspace/edit" params={{ workspace: w.name }} className="mt-2 inline-block text-sm text-accent underline">
                  Change
                </Link>
              )}
            </Card>
          </li>
        ))}
      </ul>
    </div>
  );
}
