import { useId, useMemo, useRef, useState, type KeyboardEvent } from "react";
import type { MentionCandidate } from "@/api/channels";
import { cx } from "./ui";

/** The "@prefix" being typed at the caret, if any. */
export function mentionAt(text: string, caret: number): { start: number; query: string } | null {
  const before = text.slice(0, caret);
  const m = /(^|[\s(])@([a-z0-9:_-]*)$/i.exec(before);
  if (!m) return null;
  return { start: caret - m[2].length - 1, query: m[2].toLowerCase() };
}

/** Candidates for a query: prefix matches first, then substring matches, at most 8. */
export function matchCandidates(all: MentionCandidate[], query: string): MentionCandidate[] {
  const q = query.toLowerCase();
  const pre = all.filter((c) => c.handle.toLowerCase().startsWith(q));
  const sub = all.filter((c) => !c.handle.toLowerCase().startsWith(q) && c.handle.toLowerCase().includes(q));
  return [...pre, ...sub].slice(0, 8);
}

/**
 * A compose box with @mention autocomplete (agents, people and group
 * addresses). Arrow keys pick, Enter or Tab inserts, Escape closes;
 * Ctrl/Cmd+Enter sends.
 */
export function MentionInput({
  value,
  onChange,
  onSubmit,
  candidates,
  label,
  placeholder,
  disabled,
}: {
  value: string;
  onChange: (v: string) => void;
  onSubmit: () => void;
  candidates: MentionCandidate[];
  label: string;
  placeholder?: string;
  disabled?: boolean;
}) {
  const ref = useRef<HTMLTextAreaElement>(null);
  const listId = useId();
  const [caret, setCaret] = useState(0);
  const [active, setActive] = useState(0);
  const [closed, setClosed] = useState(false);
  const at = mentionAt(value, caret);
  const options = useMemo(() => (at ? matchCandidates(candidates, at.query) : []), [at, candidates]);
  const open = !closed && !!at && options.length > 0;

  const insert = (c: MentionCandidate) => {
    if (!at) return;
    const next = `${value.slice(0, at.start)}@${c.handle} ${value.slice(caret)}`;
    onChange(next);
    const pos = at.start + c.handle.length + 2;
    requestAnimationFrame(() => {
      ref.current?.setSelectionRange(pos, pos);
      setCaret(pos);
    });
  };

  const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (open) {
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        e.preventDefault();
        setActive((a) => (a + (e.key === "ArrowDown" ? 1 : options.length - 1)) % options.length);
        return;
      }
      if (e.key === "Enter" || e.key === "Tab") {
        e.preventDefault();
        insert(options[Math.min(active, options.length - 1)]);
        return;
      }
      if (e.key === "Escape") {
        e.preventDefault();
        setClosed(true);
        return;
      }
    }
    if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      onSubmit();
    }
  };

  return (
    <div className="relative flex-1">
      <textarea
        ref={ref}
        aria-label={label}
        role="combobox"
        aria-expanded={open}
        aria-controls={listId}
        aria-autocomplete="list"
        aria-activedescendant={open ? `${listId}-${active}` : undefined}
        rows={2}
        disabled={disabled}
        value={value}
        placeholder={placeholder}
        onChange={(e) => {
          onChange(e.target.value);
          setCaret(e.target.selectionStart);
          setActive(0);
          setClosed(false);
        }}
        onSelect={(e) => setCaret(e.currentTarget.selectionStart)}
        onKeyDown={onKeyDown}
        className="min-h-10 w-full resize-y border border-border bg-surface px-3 py-2 text-sm text-fg placeholder:text-muted focus:border-fg"
      />
      {open && (
        <ul id={listId} role="listbox" aria-label="Mention" className="absolute bottom-full left-0 z-30 mb-1 w-72 max-w-full border border-border bg-surface shadow-md">
          {options.map((c, i) => (
            <li
              key={c.handle}
              id={`${listId}-${i}`}
              role="option"
              aria-selected={i === active}
              onMouseDown={(e) => {
                e.preventDefault();
                insert(c);
              }}
              className={cx("flex cursor-pointer items-baseline justify-between gap-2 px-2 py-1.5 text-sm", i === active && "bg-surface-2")}
            >
              <span className="font-mono">@{c.handle}</span>
              <span className="label truncate">
                {c.kind} · {c.detail}
              </span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
