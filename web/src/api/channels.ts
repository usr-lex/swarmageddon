// Channels (§7.8) and the warden (ADR 0011) and advisors (ADR 0010): the
// domain types the UI uses for them.

export type ChannelAccess = "read" | "write" | "readWrite";
export type DeliveryMode = "push" | "digest" | "pull";
export const deliveryModes: DeliveryMode[] = ["push", "digest", "pull"];

/** A channel as one reader sees it. */
export interface ChannelInfo {
  workspace: string;
  name: string;
  description: string;
  access: ChannelAccess;
  delivery: DeliveryMode;
  allowedDelivery: DeliveryMode[];
  unread: number;
  members: number;
}

/** One item of a reading page: a channel post, or a direct message when channel is "". */
export interface InboxItem {
  id: string;
  workspace: string;
  channel: string;
  from: string;
  fromKind: "agent" | "human";
  body: string;
  at: string;
  read: boolean;
  mentionsMe: boolean;
}

/** all, direct, mentions, or a channel name. */
export type ReadScope = "all" | "direct" | "mentions" | `#${string}`;

export interface ReadPage {
  items: InboxItem[];
  nextCursor: string;
  unreadRemaining: number;
}

export interface MentionCandidate {
  /** What goes after @: a name, or a group address like role:reviewer. */
  handle: string;
  kind: "agent" | "person" | "group";
  detail: string;
}

// Workspace wizard: channels and storm control

export interface ChannelMemberDraft {
  /** everyone, people, role:<t>, team:<t>, group:<g>, room:<n>, person:<h> */
  select: string;
  access: ChannelAccess;
}

export interface ChatChannelDraft {
  name: string;
  description: string;
  members: ChannelMemberDraft[];
  defaultDelivery: DeliveryMode;
  /** Empty: all modes. */
  allowedDelivery: DeliveryMode[];
  postsPerMinute: number;
}

export interface StormDraft {
  pushPerMinute: number;
  coalesceBytes: number;
  digestMinutes: number;
  fanoutPerMinute: number;
}

// Warden and advisors

export type WardenMode = "off" | "monitor" | "gate";
export type Severity = "low" | "medium" | "high" | "critical";
export const severities: Severity[] = ["low", "medium", "high", "critical"];

export interface WardenDraft {
  mode: WardenMode;
  /** Empty: rules only. */
  model: string;
  interruptOnHigh: boolean;
  quarantineOnHigh: boolean;
  monthlyUSD: number;
}

export interface AdvisorDraft {
  /** Empty: no advisor. */
  model: string;
  effort: string;
  maxCallsPerHour: number;
  /** Claude's built-in advisor tool; its advice can't be audited. */
  native: boolean;
}

export interface Verdict {
  id: string;
  workspace: string;
  room: string;
  layer: "rules" | "judge" | "trajectory" | "gateway";
  severity: Severity;
  /** Rule name, or "model". */
  rule: string;
  reason: string;
  evidence: string;
  /** allowed, denied, escalated, gated, interrupted, quarantined */
  action: string;
  source: "room" | "gateway";
  at: string;
  decision?: { kind: "confirmed" | "dismissed"; by: string; at: string };
}

const rank: Record<Severity, number> = { low: 1, medium: 2, high: 3, critical: 4 };

/** The most severe of the verdicts nobody has dismissed, or undefined. */
export function topSeverity(vs: Verdict[]): Severity | undefined {
  let top: Severity | undefined;
  for (const v of vs) if (v.decision?.kind !== "dismissed" && (!top || rank[v.severity] > rank[top])) top = v.severity;
  return top;
}

/** Selectors a channel member entry can use, for the wizard's picker. */
export const selectorKinds = ["everyone", "people", "role", "team", "group", "room", "person"] as const;
