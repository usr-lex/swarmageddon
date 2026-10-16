import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useEffect, type ReactNode } from "react";
import { api } from "@/api";
import { consume } from "@/api/stream";
import type { ChannelInfo } from "@/api/channels";
import { cx, ErrorText } from "@/components/ui";

/** Keeps channel lists, pages and conversations fresh while the messages area is open. */
function useItemsFeed() {
  const qc = useQueryClient();
  useEffect(() => {
    const ctl = new AbortController();
    void consume(
      (s) => api().watchItems(s),
      () => {
        void qc.invalidateQueries({ queryKey: ["channels"] });
        void qc.invalidateQueries({ queryKey: ["items"] });
      },
      ctl.signal,
    );
    return () => ctl.abort();
  }, [qc]);
}

export type Selected = { kind: "unread" | "mentions" | "all" } | { kind: "channel"; workspace: string; channel: string } | { kind: "direct"; address: string } | { kind: "none" };

const itemClass = (on: boolean) =>
  cx("flex min-h-9 items-center justify-between gap-2 px-2 text-sm", on ? "bg-fg text-bg" : "text-fg hover:bg-surface-2");

function Count({ n, on }: { n: number; on: boolean }) {
  if (!n) return null;
  return <span className={cx("font-mono text-[11px] tabular-nums", on ? "text-bg" : "text-alert")}>{n}</span>;
}

/**
 * The messages area: a calm sidebar (unread, mentions, channels by
 * workspace, direct messages with agents) beside the selected view. On a
 * phone, the sidebar shows only when nothing is selected.
 */
export function MessagesLayout({ selected, children }: { selected: Selected; children: ReactNode }) {
  useItemsFeed();
  const channels = useQuery({ queryKey: ["channels"], queryFn: () => api().listChannels() });
  const convs = useQuery({ queryKey: ["conversations"], queryFn: () => api().listConversations() });
  const byWs = new Map<string, ChannelInfo[]>();
  for (const c of channels.data ?? []) byWs.set(c.workspace, [...(byWs.get(c.workspace) ?? []), c]);
  const unread = (channels.data ?? []).reduce((a, c) => a + c.unread, 0) + (convs.data ?? []).reduce((a, c) => a + c.unread, 0);
  const is = (k: Selected["kind"]) => selected.kind === k;

  return (
    <div className="grid gap-4 md:grid-cols-[16rem_1fr]">
      <nav aria-label="Messages" className={cx("panel self-start p-2", selected.kind !== "none" && selected.kind !== "unread" && "hidden md:block")}>
        <ul className="space-y-px">
          <li>
            <Link to="/channels" search={{ scope: "unread" }} className={itemClass(is("unread"))} aria-current={is("unread") ? "page" : undefined}>
              Unread <Count n={unread} on={is("unread")} />
            </Link>
          </li>
          <li>
            <Link to="/channels" search={{ scope: "mentions" }} className={itemClass(is("mentions"))} aria-current={is("mentions") ? "page" : undefined}>
              Mentions
            </Link>
          </li>
          <li>
            <Link to="/channels" search={{ scope: "all" }} className={itemClass(is("all"))} aria-current={is("all") ? "page" : undefined}>
              Everything
            </Link>
          </li>
        </ul>
        <ErrorText error={channels.error} />
        {[...byWs.entries()].map(([ws, list]) => (
          <section key={ws} className="mt-3">
            <h2 className="label px-2 pb-1">Channels · {ws}</h2>
            <ul className="space-y-px" data-testid={`channels-${ws}`}>
              {list.map((c) => {
                const on = selected.kind === "channel" && selected.workspace === ws && selected.channel === c.name;
                return (
                  <li key={c.name}>
                    <Link
                      to="/channels/$workspace/$channel"
                      params={{ workspace: ws, channel: c.name }}
                      className={cx(itemClass(on), !on && c.unread > 0 && "font-semibold")}
                      aria-current={on ? "page" : undefined}
                    >
                      <span className="truncate">
                        <span className="text-muted">#</span>
                        {c.name}
                      </span>
                      <Count n={c.unread} on={on} />
                    </Link>
                  </li>
                );
              })}
            </ul>
          </section>
        ))}
        <section className="mt-3">
          <h2 className="label px-2 pb-1">Direct messages</h2>
          <ul className="max-h-72 space-y-px overflow-auto">
            {convs.data?.map((c) => {
              const on = selected.kind === "direct" && selected.address === c.address;
              return (
                <li key={c.address}>
                  <Link to="/inbox/$address" params={{ address: c.address }} className={cx(itemClass(on), !on && c.unread > 0 && "font-semibold")} aria-current={on ? "page" : undefined}>
                    <span className="truncate">{c.displayName}</span>
                    <Count n={c.unread} on={on} />
                  </Link>
                </li>
              );
            })}
          </ul>
        </section>
      </nav>
      <div className="min-w-0">{children}</div>
    </div>
  );
}
