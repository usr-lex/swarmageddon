import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useState } from "react";
import { api } from "@/api";
import type { Approval, Decision } from "@/api/types";
import { useMe } from "@/components/Shell";
import { Badge, Button, Card, Empty, ErrorText, PageHeader, Spinner, Textarea, cx } from "@/components/ui";
import { ago } from "@/lib/format";
import { useApprovals } from "@/lib/live";

/** The approvals inbox (§8.5). `focus` is the deep link from a push notification. */
export function ApprovalsPage({ focus }: { focus?: string }) {
  const list = useApprovals();
  const focused = useQuery({ queryKey: ["approval", focus], queryFn: () => api().getApproval(focus!), enabled: !!focus });
  const others = (list.data ?? []).filter((a) => a.id !== focus);
  return (
    <div className="mx-auto max-w-3xl">
      <PageHeader title="Approvals" subtitle={list.data ? `${list.data.length} waiting` : undefined} />
      {focus && (
        <div className="mb-6">
          {focused.isPending && <Spinner label="Loading approval" />}
          <ErrorText error={focused.error} />
          {focused.data && <ApprovalCard a={focused.data} highlight />}
          {others.length > 0 && <h2 className="mt-6 mb-2 text-sm font-semibold text-muted">Also waiting</h2>}
        </div>
      )}
      {list.isPending && <Spinner label="Loading approvals" />}
      <ErrorText error={list.error} />
      {list.data && list.data.length === 0 && !focus && <Empty>Nothing is waiting for you.</Empty>}
      <ul className="space-y-3" aria-label="Pending approvals">
        {others.map((a) => (
          <li key={a.id}>
            <ApprovalCard a={a} />
          </li>
        ))}
      </ul>
    </div>
  );
}

function looksLikeDiff(s: string) {
  return /^(---|\+\+\+|@@) /m.test(s);
}

function Detail({ text }: { text: string }) {
  let shown = text;
  try {
    const j = JSON.parse(text);
    shown = typeof j === "object" ? JSON.stringify(j, null, 2) : text;
  } catch {
    /* not JSON */
  }
  const diff = looksLikeDiff(shown);
  return (
    <pre className="max-h-80 overflow-auto rounded-md bg-surface-2 p-2 text-xs whitespace-pre-wrap break-all" aria-label="Request detail">
      {diff
        ? shown.split("\n").map((l, i) => (
            <span key={i} className={cx("block", l.startsWith("+") && !l.startsWith("+++") && "bg-ok/15 text-ok", l.startsWith("-") && !l.startsWith("---") && "bg-bad/15 text-bad", l.startsWith("@@") && "text-info")}>
              {l || " "}
            </span>
          ))
        : shown}
    </pre>
  );
}

function ApprovalCard({ a, highlight }: { a: Approval; highlight?: boolean }) {
  const me = useMe();
  const canAnswer = me.data?.role === "admin" || me.data?.role === "operator";
  const qc = useQueryClient();
  const [denying, setDenying] = useState(false);
  const [message, setMessage] = useState("");
  const answer = useMutation({
    mutationFn: (d: Decision) => api().answerApproval(a.id, d, d === "deny" ? message : ""),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ["approvals"] });
      await qc.invalidateQueries({ queryKey: ["approval", a.id] });
    },
  });
  const done = a.answer || a.withdrawn || answer.isSuccess;
  return (
    <Card className={cx(highlight && "border-accent")}>
      <article aria-label={`Approval from ${a.room}`} data-testid="approval" data-id={a.id}>
        <header className="flex flex-wrap items-start justify-between gap-2">
          <div className="min-w-0">
            <p className="text-sm text-muted">
              <Link to="/rooms/$workspace/$room" params={{ workspace: a.workspace, room: a.room }} search={{ tab: "agent" }} className="font-medium text-fg hover:underline">
                {a.room}
              </Link>{" "}
              in {a.workspace} · {a.cli === "claude" ? "Claude Code" : "ore"} · {ago(a.requestedAt)}
            </p>
            <h3 className="mt-1 font-semibold break-words">
              <Badge tone="warn" className="mr-2 align-middle">
                {a.tool}
              </Badge>
              {a.summary}
            </h3>
          </div>
        </header>
        {a.reason && <p className="mt-2 text-sm"><span className="text-muted">Reason: </span>{a.reason}</p>}
        <div className="mt-2">
          <Detail text={a.detail} />
        </div>
        {a.matchingRules.length > 0 && (
          <p className="mt-2 text-xs text-muted">
            Matching rules: <span className="font-mono">{a.matchingRules.join("; ")}</span>
          </p>
        )}
        {done ? (
          <p role="status" className="mt-3 text-sm font-medium">
            {a.withdrawn
              ? `Withdrawn: ${a.withdrawn}`
              : a.answer
                ? `Answered (${a.answer.decision}) by ${a.answer.answeredBy}`
                : `Answered: ${answer.variables}`}
          </p>
        ) : !canAnswer ? (
          <p className="mt-3 text-sm text-muted">Viewers can't answer approvals.</p>
        ) : denying ? (
          <form
            className="mt-3 space-y-2"
            onSubmit={(e) => {
              e.preventDefault();
              answer.mutate("deny");
            }}
          >
            <label className="text-sm font-medium" htmlFor={`deny-${a.id}`}>
              Message to the agent
            </label>
            <Textarea id={`deny-${a.id}`} rows={3} value={message} onChange={(e) => setMessage(e.target.value)} placeholder="Why not, and what to do instead" className="font-sans" autoFocus />
            <div className="grid grid-cols-2 gap-2 sm:flex">
              <Button onClick={() => setDenying(false)}>Cancel</Button>
              <Button type="submit" variant="danger" busy={answer.isPending}>
                Deny
              </Button>
            </div>
          </form>
        ) : (
          <div className="mt-3 grid gap-2 sm:flex">
            <Button variant="primary" className="min-h-12 sm:min-h-10" busy={answer.isPending && answer.variables === "allow"} onClick={() => answer.mutate("allow")}>
              Allow once
            </Button>
            {a.sessionScopeAvailable && (
              <Button className="min-h-12 sm:min-h-10" busy={answer.isPending && answer.variables === "allowForSession"} onClick={() => answer.mutate("allowForSession")}>
                Allow for this session
              </Button>
            )}
            <Button variant="danger" className="min-h-12 sm:min-h-10" onClick={() => setDenying(true)}>
              Deny…
            </Button>
          </div>
        )}
        <ErrorText error={answer.error} />
      </article>
    </Card>
  );
}
