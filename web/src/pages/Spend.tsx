import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { api } from "@/api";
import { Card, ErrorText, Meter, PageHeader, Select, Spinner } from "@/components/ui";
import { usd } from "@/lib/format";
import { groupSpend } from "@/lib/spend";

type Period = "month" | "week" | "day";
type By = "room" | "workspace" | "model";

export function SpendPage() {
  const [period, setPeriod] = useState<Period>("month");
  const [by, setBy] = useState<By>("room");
  const q = useQuery({ queryKey: ["spend", period], queryFn: () => api().getSpend(period) });
  const groups = q.data ? groupSpend(q.data, by) : [];
  const total = groups.reduce((a, g) => a + g.usd, 0);
  const max = Math.max(1, ...groups.map((g) => g.usd));
  return (
    <div>
      <PageHeader
        title="Spend"
        subtitle={q.data ? `${usd(total)} this ${period}` : undefined}
        actions={
          <>
            <Select aria-label="Period" value={period} onChange={(e) => setPeriod(e.target.value as Period)} className="w-auto">
              <option value="day">Today</option>
              <option value="week">This week</option>
              <option value="month">This month</option>
            </Select>
            <Select aria-label="Group by" value={by} onChange={(e) => setBy(e.target.value as By)} className="w-auto">
              <option value="room">By room</option>
              <option value="workspace">By workspace</option>
              <option value="model">By model</option>
            </Select>
          </>
        }
      />
      {q.isPending && <Spinner label="Loading" />}
      <ErrorText error={q.error} />
      {q.data && (
        <Card>
          <table className="w-full text-left text-sm">
            <caption className="sr-only">Spend {by}</caption>
            <thead className="text-xs text-muted">
              <tr>
                <th scope="col" className="py-1 font-medium capitalize">
                  {by}
                </th>
                <th scope="col" className="py-1 font-medium">
                  Spend
                </th>
                <th scope="col" className="hidden py-1 font-medium sm:table-cell">
                  Tokens in / out
                </th>
                <th scope="col" className="w-1/3 py-1 font-medium">
                  <span className="sr-only">Share</span>
                </th>
              </tr>
            </thead>
            <tbody className="divide-y divide-border">
              {groups.map((g) => (
                <tr key={g.key}>
                  <td className="py-2 pr-2 break-all">{g.key}</td>
                  <td className="py-2 pr-2 whitespace-nowrap">
                    {usd(g.usd)}
                    {g.budgetUSD > 0 && <span className="text-muted"> / {usd(g.budgetUSD)}</span>}
                  </td>
                  <td className="hidden py-2 pr-2 sm:table-cell">
                    {g.inputTokens.toLocaleString()} / {g.outputTokens.toLocaleString()}
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
