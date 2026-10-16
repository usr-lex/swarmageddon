import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useParams, useSearch } from "@tanstack/react-router";
import { useEffect, useMemo, useRef, useState } from "react";
import { api } from "@/api";
import type { DeliveryMode, InboxItem, ReadScope } from "@/api/channels";
import { MentionInput } from "@/components/MentionInput";
import { MessageBody } from "@/components/MessageBody";
import { useMe } from "@/components/Shell";
import { Badge, Button, Empty, ErrorText, Spinner, cx } from "@/components/ui";
import { ago } from "@/lib/format";
import { Segmented } from "../observatory/common";
import { MessagesLayout } from "./MessagesLayout";

const PAGE = 20;

export type FeedScope = "unread" | "mentions" | "all";

/** Pages of items for one workspace and scope, newest last, loading older pages on demand. */
function useItems(workspace: string, scope: ReadScope, unreadOnly: boolean, enabled = true) {
  return useInfiniteQuery({
    queryKey: ["items", workspace, scope, unreadOnly],
    queryFn: ({ pageParam }) => api().readMessages(workspace, scope, { unreadOnly, limit: PAGE, cursor: pageParam || undefined }),
    initialPageParam: "",
    getNextPageParam: (last) => last.nextCursor || undefined,
    enabled,
  });
}

function Item({ it, me, showChannel }: { it: InboxItem; me?: string; showChannel?: boolean }) {
  return (
    <li
      className={cx("border-l-2 py-2 pr-2 pl-3", it.mentionsMe ? "border-alert bg-alert/5" : it.read ? "border-transparent" : "border-viz-1")}
      data-testid="item"
      data-mentions-me={it.mentionsMe || undefined}
    >
      <div className="flex flex-wrap items-baseline gap-x-2 text-xs">
        <span className="font-mono font-medium">{it.from}</span>
        {it.fromKind === "human" && <Badge>person</Badge>}
        {showChannel &&
          (it.channel ? (
            <Link to="/channels/$workspace/$channel" params={{ workspace: it.workspace, channel: it.channel }} className="text-muted hover:underline">
              #{it.channel} · {it.workspace}
            </Link>
          ) : it.fromKind === "agent" ? (
            <Link to="/inbox/$address" params={{ address: it.from }} className="text-muted hover:underline">
              direct · {it.workspace}
            </Link>
          ) : (
            <span className="text-muted">direct · {it.workspace}</span>
          ))}
        <time className="text-muted" dateTime={it.at} title={new Date(it.at).toLocaleString()}>
          {ago(it.at)}
        </time>
        {!it.read && <span className="sr-only">unread</span>}
      </div>
      <div className="text-sm">
        <MessageBody body={it.body} me={me} />
      </div>
    </li>
  );
}

/** The unified view (§7.8): unread, mentions or everything, across channels and direct messages, paginated. */
export function ChannelsPage() {
  const { scope } = useSearch({ from: "/channels" });
  const me = useMe();
  const channels = useQuery({ queryKey: ["channels"], queryFn: () => api().listChannels() });
  const workspaces = useMemo(() => [...new Set((channels.data ?? []).map((c) => c.workspace))], [channels.data]);
  return (
    <MessagesLayout selected={{ kind: scope }}>
      <header className="mb-3">
        <p className="label">Messages</p>
        <h1 className="text-xl font-semibold tracking-tight">{scope === "unread" ? "Unread" : scope === "mentions" ? "Mentions" : "Everything"}</h1>
      </header>
      {channels.isPending && <Spinner label="Loading" />}
      <ErrorText error={channels.error} />
      {channels.data && workspaces.length === 0 && <Empty>No channels yet. Workspaces get channels in the workspace wizard.</Empty>}
      <div className="space-y-4">
        {workspaces.map((ws) => (
          <Feed key={ws} workspace={ws} scope={scope} me={me.data?.handle} multi={workspaces.length > 1} />
        ))}
      </div>
    </MessagesLayout>
  );
}

function Feed({ workspace, scope, me, multi }: { workspace: string; scope: FeedScope; me?: string; multi: boolean }) {
  const qc = useQueryClient();
  const q = useItems(workspace, scope === "mentions" ? "mentions" : "all", scope === "unread");
  const items = q.data?.pages.flatMap((p) => p.items) ?? [];
  const sorted = [...items].sort((a, b) => b.at.localeCompare(a.at));
  const remaining = q.data?.pages[0]?.unreadRemaining ?? 0;
  const markAll = useMutation({
    mutationFn: () => api().markRead(workspace, "all"),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["items"] });
      void qc.invalidateQueries({ queryKey: ["channels"] });
    },
  });
  return (
    <section className="panel p-3" aria-label={`Workspace ${workspace}`}>
      <div className="mb-2 flex items-baseline justify-between gap-2">
        <h2 className="label !text-fg">{multi ? workspace : "Newest first"}</h2>
        {scope === "unread" && remaining > 0 && (
          <Button className="!min-h-8 font-mono text-[11px] uppercase" busy={markAll.isPending} onClick={() => markAll.mutate()}>
            Mark all read ({remaining})
          </Button>
        )}
      </div>
      {q.isPending && <Spinner label="Loading" />}
      <ErrorText error={q.error} />
      {q.data && sorted.length === 0 && <p className="text-sm text-muted">{scope === "unread" ? "All caught up." : "Nothing here."}</p>}
      <ol className="divide-y divide-border">
        {sorted.map((it) => (
          <Item key={it.id} it={it} me={me} showChannel />
        ))}
      </ol>
      {q.hasNextPage && (
        <Button className="mt-2" busy={q.isFetchingNextPage} onClick={() => q.fetchNextPage()}>
          Older
        </Button>
      )}
    </section>
  );
}

const deliveryHelp: Record<DeliveryMode, string> = {
  push: "every post reaches you",
  digest: "one summary per interval",
  pull: "nothing is pushed; read here",
};

/** One channel: posts, compose with @mention autocomplete, and this person's delivery mode. */
export function ChannelView() {
  const { workspace, channel } = useParams({ from: "/channels/$workspace/$channel" });
  const me = useMe();
  const qc = useQueryClient();
  const channels = useQuery({ queryKey: ["channels"], queryFn: () => api().listChannels() });
  const info = channels.data?.find((c) => c.workspace === workspace && c.name === channel);
  const q = useItems(workspace, `#${channel}`, false);
  const cands = useQuery({ queryKey: ["mentionCandidates", workspace], queryFn: () => api().mentionCandidates(workspace), staleTime: 60_000 });
  const [body, setBody] = useState("");
  const items = useMemo(() => {
    const all = q.data?.pages.flatMap((p) => p.items) ?? [];
    return [...all].sort((a, b) => a.at.localeCompare(b.at));
  }, [q.data]);
  const end = useRef<HTMLDivElement>(null);
  const newest = items[items.length - 1]?.id;
  useEffect(() => end.current?.scrollIntoView?.({ block: "end" }), [newest]);

  // Viewing a channel reads it.
  useEffect(() => {
    if (!newest || !info?.unread) return;
    void api()
      .markRead(workspace, `#${channel}`, newest)
      .then(() => qc.invalidateQueries({ queryKey: ["channels"] }));
  }, [newest, info?.unread, workspace, channel, qc]);

  const send = useMutation({
    mutationFn: () => api().post(workspace, channel, body),
    onSuccess: () => {
      setBody("");
      void qc.invalidateQueries({ queryKey: ["items", workspace, `#${channel}`] });
    },
  });
  const delivery = useMutation({
    mutationFn: (m: DeliveryMode) => api().setDelivery(workspace, channel, m),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["channels"] }),
  });
  const canWrite = info?.access === "write" || info?.access === "readWrite";
  const canRead = info?.access !== "write";

  return (
    <MessagesLayout selected={{ kind: "channel", workspace, channel }}>
      <section className="panel flex min-h-[60vh] flex-col" aria-label={`#${channel}`}>
        <header className="flex flex-wrap items-center justify-between gap-2 border-b border-border px-3 py-2">
          <div className="min-w-0">
            <Link to="/channels" search={{ scope: "unread" }} className="label md:hidden">
              ← Messages
            </Link>
            <h1 className="font-semibold">
              <span className="text-muted">#</span>
              {channel} <span className="label">· {workspace}</span>
            </h1>
            {info && (
              <p className="text-xs text-muted">
                {info.description} · {info.members} member entries · you can {info.access === "readWrite" ? "read and post" : info.access === "write" ? "post only" : "read only"}
              </p>
            )}
          </div>
          {info && (
            <div className="flex items-center gap-2" data-testid="delivery">
              <span className="label" title={deliveryHelp[info.delivery]}>
                Delivery
              </span>
              <Segmented label="Delivery for this channel" value={info.delivery} options={info.allowedDelivery} onChange={(m) => delivery.mutate(m)} />
            </div>
          )}
        </header>
        <div className="flex-1 overflow-y-auto px-1" role="log" aria-live="polite" aria-label="Posts">
          {q.hasNextPage && (
            <div className="p-2 text-center">
              <Button busy={q.isFetchingNextPage} onClick={() => q.fetchNextPage()}>
                Older posts
              </Button>
            </div>
          )}
          {q.isPending && <Spinner label="Loading posts" />}
          {!canRead && info && <p className="p-3 text-sm text-muted">This is a report-in channel: you can post, but not read it.</p>}
          {q.data && items.length === 0 && canRead && <p className="p-3 text-sm text-muted">No posts yet.</p>}
          <ol className="divide-y divide-border">
            {items.map((it) => (
              <Item key={it.id} it={it} me={me.data?.handle} />
            ))}
          </ol>
          <div ref={end} />
        </div>
        <ErrorText error={q.error ?? delivery.error} />
        {canWrite && (
          <form
            className="flex items-end gap-2 border-t border-border p-2"
            onSubmit={(e) => {
              e.preventDefault();
              if (body.trim()) send.mutate();
            }}
          >
            <MentionInput
              value={body}
              onChange={setBody}
              onSubmit={() => body.trim() && send.mutate()}
              candidates={cands.data ?? []}
              label={`Post to #${channel}`}
              placeholder={`Post to #${channel} — @name pushes to that agent (Ctrl+Enter sends)`}
            />
            <Button type="submit" variant="primary" busy={send.isPending} disabled={!body.trim()}>
              Post
            </Button>
          </form>
        )}
        {send.data && send.data.mentioned.length > 0 && (
          <p role="status" className="px-3 pb-2 text-xs text-muted">
            Pushed to {send.data.mentioned.join(", ")}.
          </p>
        )}
        <ErrorText error={send.error} />
      </section>
    </MessagesLayout>
  );
}
