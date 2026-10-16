import type { ReactNode } from "react";
import { cx } from "./ui";

/** Renders a message body as text, with @mentions and `code` marked; `me` is highlighted. */
export function MessageBody({ body, me }: { body: string; me?: string }) {
  const out: ReactNode[] = [];
  const re = /(`[^`]+`|@[a-z][a-z0-9-]*(?::[a-z0-9_-]+)?)/gi;
  let last = 0;
  for (let m = re.exec(body); m; m = re.exec(body)) {
    if (m.index > last) out.push(body.slice(last, m.index));
    const t = m[0];
    if (t.startsWith("`")) {
      out.push(
        <code key={m.index} className="bg-surface-2 px-1 font-mono text-[0.9em]">
          {t.slice(1, -1)}
        </code>,
      );
    } else {
      const mine = !!me && t.slice(1).toLowerCase() === me.toLowerCase();
      out.push(
        <span key={m.index} className={cx("font-medium", mine ? "bg-alert/15 px-0.5 text-alert" : "text-viz-1")} data-mention={t.slice(1)}>
          {t}
        </span>,
      );
    }
    last = m.index + t.length;
  }
  if (last < body.length) out.push(body.slice(last));
  return <p className="whitespace-pre-wrap break-words">{out}</p>;
}
