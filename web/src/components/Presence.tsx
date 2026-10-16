import type { Presence } from "@/api/types";
import { Badge, type Tone } from "./ui";

export const presenceInfo: Record<Presence, { label: string; tone: Tone; icon: string }> = {
  waitingOnHuman: { label: "Waiting on you", tone: "warn", icon: "●" },
  crashed: { label: "Crashed", tone: "bad", icon: "✕" },
  working: { label: "Working", tone: "ok", icon: "▶" },
  idle: { label: "Idle", tone: "info", icon: "◌" },
  offline: { label: "Offline", tone: "muted", icon: "○" },
};

export function PresenceBadge({ presence }: { presence: Presence }) {
  const p = presenceInfo[presence];
  return (
    <Badge tone={p.tone}>
      <span aria-hidden>{p.icon}</span>
      {p.label}
    </Badge>
  );
}
