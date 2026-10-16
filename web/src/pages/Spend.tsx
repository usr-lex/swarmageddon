import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { api } from "@/api";
import { Badge, Card, ErrorText, Meter, PageHeader, Select, Spinner } from "@/components/ui";
import { ago, usd } from "@/lib/format";
import { groupSpend } from "@/lib/spend";

type By = "room" | "workspace" | "model";

/** Spend this budget period (§10.2), by room, workspace or model. */
export function SpendPage() {
  const [by, setBy] = useState<By>("room");
  const q = useQuery({ queryKey: ["spend"], queryFn: () => api().getSpend(), refetchInterval: 60_000 });
  const groups = q.data ? groupSpend(q.data, by) : [];
  const total = q.data?.workspaces.reduce((a, w) => a + w.usd, 0) ?? 0;
  const max = Math.max(1, ...groups.map((g) => g.usd));
  return (
    <div>
      <PageHeader
        title="Spend"
        subtitle={q.data ? `${usd(total)} this budget period · updated ${ago(q.data.updatedAt)}` : undefined}
        actions={
          <Select aria-label="Group by" value={by} onChange={(e) => setBy(e.target.value as By)} className="w-auto">
            <option value="room">By room</option>
            <option value="workspace">By workspace</option>
            <option value="model">By model</option>
          </Select>
        }
      />
      {q.isPending && <Spinner label="Loading" />}
      <ErrorText error={q.error} />
      {q.data && !q.data.available && <p className="mb-3 text-sm text-warn">The model gateway isn't reachable yet; numbers may be stale.</p>}
      {q.data && (
        <Card>
          <table className="w-full text-left text-sm">
            <caption className="sr-only">Spend by {by}</caption>
            <thead>
              <tr>
                <th scope="col" className="label py-1 font-normal">
                  {by}
                </th>
                <th scope="col" className="label py-1 font-normal">
                  Spend
                </th>
                <th scope="col" className="w-1/3 py-1">
                  <span className="sr-only">Share of budget</span>
                </th>
              </tr>
            </thead>
            <tbody className="divide-y divide-border">
              {groups.map((g) => (
                <tr key={g.key}>
                  <td className="py-2 pr-2 font-mono text-xs break-all">{g.key}</td>
                  <td className="py-2 pr-2 whitespace-nowrap tabular-nums">
                    {usd(g.usd)}
                    {g.budgetUSD > 0 && <span className="text-muted"> / {usd(g.budgetUSD)}</span>}
                    {g.exhausted && (
                      <Badge tone="alert" className="ml-2">
                        exhausted
                      </Badge>
                    )}
                  </td>
                  <td className="py-2">
                    <Meter value={g.usd} max={g.budgetUSD > 0 ? g.budgetUSD : max} label={`${g.key} spend`} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </Card>
      )}
    </div>
  );
}
