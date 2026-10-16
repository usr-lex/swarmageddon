import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";
import { api } from "@/api";
import { consume } from "@/api/stream";
import type { ChatMessage } from "@/api/types";
import { PresenceBadge } from "@/components/Presence";
import { useMe } from "@/components/Shell";
import { Button, Empty, ErrorText, PageHeader, Spinner, Textarea, cx } from "@/components/ui";
import { ago } from "@/lib/format";

/** Messages between people and agents, one conversation per agent. */
export function InboxPage({ address }: { address?: string }) {
  const qc = useQueryClient();
  const convs = useQuery({ queryKey: ["conversations"], queryFn: () => api().listConversations() });
  useEffect(() => {
    const ctl = new AbortController();
    void consume(
      (s) => api().watchMessages(s),
      (m) => {
        for (const a of [m.from, m.to]) qc.setQueryData<ChatMessage[]>(["messages", a], (old) => (old && !old.some((x) => x.id === m.id) ? [...old, m] : old));
        void qc.invalidateQueries({ queryKey: ["conversations"] });
      },
      ctl.signal,
    );
    return () => ctl.abort();
  }, [qc]);

  return (
    <div>
      <PageHeader title="Inbox" />
      <div className="grid gap-4 md:grid-cols-[18rem_1fr]">
        <nav aria-label="Conversations" className={cx(address && "hidden md:block")}>
          {convs.isPending && <Spinner label="Loading" />}
          <ErrorText error={convs.error} />
          {convs.data?.length === 0 && <Empty>No agents yet.</Empty>}
          <ul className="divide-y divide-border rounded-lg border border-border bg-surface">
            {convs.data?.map((c) => (
              <li key={c.address}>
                <Link
                  to="/inbox/$address"
                  params={{ address: c.address }}
                  className={cx("block min-h-12 px-3 py-2 hover:bg-surface-2", c.address === address && "bg-surface-2")}
                  aria-current={c.address === address ? "page" : undefined}
                >
                  <div className="flex items-center justify-between gap-2">
                    <span className="font-medium">{c.displayName}</span>
                    <PresenceBadge presence={c.presence} />
                  </div>
                  <p className="truncate text-xs text-muted">{c.lastMessage ? c.lastMessage.body : c.address}</p>
                </Link>
              </li>
            ))}
          </ul>
        </nav>
        {address ? <Conversation address={address} /> : <div className="hidden md:block"><Empty>Pick a conversation.</Empty></div>}
      </div>
    </div>
  );
}

function Conversation({ address }: { address: string }) {
  const me = useMe();
  const qc = useQueryClient();
  const msgs = useQuery({ queryKey: ["messages", address], queryFn: () => api().listMessages(address) });
  const [body, setBody] = useState("");
  const end = useRef<HTMLDivElement>(null);
  const send = useMutation({
    mutationFn: () => api().sendMessage(address, body),
    onSuccess: (m) => {
      setBody("");
      qc.setQueryData<ChatMessage[]>(["messages", address], (old) => (old && !old.some((x) => x.id === m.id) ? [...old, m] : old));
    },
  });
  useEffect(() => end.current?.scrollIntoView?.({ block: "end" }), [msgs.data?.length]);
  const canSend = me.data?.role !== "viewer";
  return (
    <section aria-label={`Conversation with ${address}`} className="flex min-h-[60vh] flex-col rounded-lg border border-border bg-surface">
      <header className="flex items-center gap-2 border-b border-border px-3 py-2">
        <Link to="/inbox" className="min-h-10 content-center px-1 md:hidden" aria-label="Back to conversations">
          ←
        </Link>
        <h2 className="font-semibold">{address}</h2>
      </header>
      <div className="flex-1 space-y-2 overflow-y-auto p-3" role="log" aria-live="polite">
        {msgs.isPending && <Spinner label="Loading messages" />}
        {msgs.data?.length === 0 && <p className="text-sm text-muted">No messages yet.</p>}
        {msgs.data?.map((m) => {
          const mine = m.fromKind === "human";
          return (
            <div key={m.id} className={cx("max-w-[85%] rounded-lg px-3 py-2 text-sm", mine ? "ml-auto bg-accent text-accent-fg" : "bg-surface-2")}>
              <p className="whitespace-pre-wrap break-words">{m.body}</p>
              <p className={cx("mt-1 text-[11px]", mine ? "opacity-80" : "text-muted")}>
                {mine ? m.from.split("@")[0] : m.from} · {ago(m.at)}
              </p>
            </div>
          );
        })}
        <div ref={end} />
      </div>
      {canSend && (
        <form
          className="flex items-end gap-2 border-t border-border p-2"
          onSubmit={(e) => {
            e.preventDefault();
            if (body.trim()) send.mutate();
          }}
        >
          <label htmlFor="compose" className="sr-only">
            Message
          </label>
          <Textarea
            id="compose"
            rows={2}
            value={body}
            onChange={(e) => setBody(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
                e.preventDefault();
                if (body.trim()) send.mutate();
              }
            }}
            placeholder="Message (Ctrl+Enter to send)"
            className="font-sans"
          />
          <Button type="submit" variant="primary" busy={send.isPending} disabled={!body.trim()}>
            Send
          </Button>
        </form>
      )}
      <ErrorText error={send.error} />
    </section>
  );
}
