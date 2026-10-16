import { useCallback, useLayoutEffect, useRef, useState, type ReactNode } from "react";
import type { EdgeKind, ObsWindow } from "@/api/observatory";
import type { Presence } from "@/api/types";
import { cx } from "@/components/ui";

/** An instrument panel: hairline frame, corner ticks, a quiet mono label. */
export function Panel({
  label,
  meta,
  actions,
  children,
  className,
  id,
}: {
  label: string;
  meta?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
  className?: string;
  id?: string;
}) {
  return (
    <section className={cx("panel flex min-w-0 flex-col p-3", className)} aria-labelledby={id} data-testid={id}>
      <header className="mb-2 flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
        <h2 id={id} className="label !text-fg">
          {label}
        </h2>
        {meta && <span className="label">{meta}</span>}
        {actions && <div className="ml-auto flex flex-wrap items-center gap-2">{actions}</div>}
      </header>
      <div className="relative min-w-0 flex-1">{children}</div>
    </section>
  );
}

export const windows: ObsWindow[] = ["1h", "24h", "7d", "30d"];

/** A segmented control in the panel register. */
export function Segmented<T extends string>({ value, options, onChange, label }: { value: T; options: readonly T[]; onChange: (v: T) => void; label: string }) {
  return (
    <div role="radiogroup" aria-label={label} className="flex border border-border">
      {options.map((o) => (
        <button
          key={o}
          type="button"
          role="radio"
          aria-checked={o === value}
          onClick={() => onChange(o)}
          className={cx("min-h-8 px-2 font-mono text-[11px] tracking-wider uppercase", o === value ? "bg-fg text-bg" : "text-muted hover:text-fg")}
        >
          {o}
        </button>
      ))}
    </div>
  );
}

export const kindColor: Record<EdgeKind, string> = {
  open: "var(--viz-neutral)",
  mediated: "var(--viz-1)",
  human: "var(--viz-2)",
};

export const kindLabel: Record<EdgeKind, string> = { open: "Open link", mediated: "Mediated crossing", human: "Human" };

/** Status colors for presence; always shown with a label or legend. */
export const presenceColor: Record<Presence, string> = {
  waitingOnHuman: "var(--alert)",
  crashed: "var(--bad)",
  working: "var(--ok)",
  idle: "var(--info)",
  offline: "var(--viz-neutral)",
};

export const groupColor = (g: string, groups: string[]) => (groups.indexOf(g) % 2 === 1 ? "var(--viz-2)" : "var(--viz-1)");

export function LegendItem({ color, label, dashed, shape = "line" }: { color: string; label: string; dashed?: boolean; shape?: "line" | "rect" | "dot" }) {
  return (
    <span className="inline-flex items-center gap-1.5">
      <svg width="16" height="10" aria-hidden>
        {shape === "line" && <line x1="1" y1="5" x2="15" y2="5" stroke={color} strokeWidth="2" strokeDasharray={dashed ? "3 2" : undefined} />}
        {shape === "rect" && <rect x="2" y="1" width="12" height="8" fill={color} />}
        {shape === "dot" && <circle cx="8" cy="5" r="4" fill={color} />}
      </svg>
      <span className="label">{label}</span>
    </span>
  );
}

export function Legend({ children }: { children: ReactNode }) {
  return <div className="flex flex-wrap gap-x-3 gap-y-1">{children}</div>;
}

export interface Tip {
  x: number;
  y: number;
  content: ReactNode;
}

/** One tooltip per chart, positioned inside the chart's relative container. */
export function useTip() {
  const [tip, setTip] = useState<Tip | null>(null);
  const ref = useRef<HTMLDivElement>(null);
  const show = useCallback((e: { clientX: number; clientY: number }, content: ReactNode) => {
    const r = ref.current?.getBoundingClientRect();
    if (!r) return;
    setTip({ x: e.clientX - r.left, y: e.clientY - r.top, content });
  }, []);
  /** For keyboard focus: anchor at the element's center. */
  const showAt = useCallback((el: Element, content: ReactNode) => {
    const r = ref.current?.getBoundingClientRect();
    const b = el.getBoundingClientRect();
    if (!r) return;
    setTip({ x: b.left + b.width / 2 - r.left, y: b.top - r.top, content });
  }, []);
  const hide = useCallback(() => setTip(null), []);
  const node = tip && (
    <div
      role="tooltip"
      className="pointer-events-none absolute z-20 max-w-64 border border-border bg-surface px-2 py-1 text-xs shadow-sm"
      style={{
        left: Math.min(tip.x + 12, (ref.current?.clientWidth ?? 9999) - 180),
        top: Math.max(0, tip.y - 8),
      }}
    >
      {tip.content}
    </div>
  );
  return { ref, show, showAt, hide, node };
}

/** Width of a container, for charts that lay out in pixels. */
export function useWidth<T extends HTMLElement>(fallback = 600) {
  const ref = useRef<T>(null);
  const [w, setW] = useState(fallback);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    setW(el.clientWidth || fallback);
    const ro = new ResizeObserver(([e]) => setW(Math.max(200, Math.floor(e.contentRect.width))));
    ro.observe(el);
    return () => ro.disconnect();
  }, [fallback]);
  return [ref, w] as const;
}

export function TipRow({ value, label, color }: { value: ReactNode; label: ReactNode; color?: string }) {
  return (
    <div className="flex items-center gap-2">
      {color && (
        <svg width="10" height="6" aria-hidden>
          <line x1="0" y1="3" x2="10" y2="3" stroke={color} strokeWidth="2" />
        </svg>
      )}
      <strong className="tabular-nums">{value}</strong>
      <span className="text-muted">{label}</span>
    </div>
  );
}

/** Hours or minutes since `iso`, compact: "3h 12m". */
export function since(iso: string, now = Date.now()): string {
  const m = Math.max(0, Math.round((now - Date.parse(iso)) / 60000));
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 48) return `${h}h ${m % 60}m`;
  return `${Math.floor(h / 24)}d ${h % 24}h`;
}
