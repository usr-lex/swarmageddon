// Observatory types (§8.7): the workspace home page's data.

import type { Presence } from "./types";

export type EdgeKind = "open" | "mediated" | "human" | "channel";
export type HandoffKind = "reviewRequested" | "assigned" | "mentioned";
export type ObsWindow = "1h" | "24h" | "7d" | "30d";

/** Messages from one party to another in the window. Parties are agent names, or "@<handle>" for people. */
export interface MessageEdge {
  from: string;
  to: string;
  kind: EdgeKind;
  count: number;
}

/** Forge handoffs: review requested, assigned, mentioned. */
export interface HandoffEdge {
  from: string;
  to: string;
  kind: HandoffKind;
  count: number;
}

export interface ObsAgent {
  name: string;
  group: string;
  role: string;
  presence: Presence;
  /** Events in the window, for node size. */
  activity: number;
}

export interface Edges {
  agents: ObsAgent[];
  /** People who messaged agents in the window, as "@<handle>". */
  humans: string[];
  /** Channels posted to in the window, as "#<name>"; their edges have kind "channel". */
  channels: string[];
  messages: MessageEdge[];
  handoffs: HandoffEdge[];
}

/** One message's metadata (never the body). */
export interface MessageMeta {
  id: string;
  at: string;
  from: string;
  to: string;
  kind: EdgeKind;
  /** Size, when known. */
  bytes?: number;
}

export interface NowRow {
  name: string;
  role: string;
  group: string;
  presence: Presence;
  currentTask: string;
  lastTouched?: { kind: "pr" | "issue"; repo: string; number: number; title: string };
  /** When the agent entered its current presence. */
  since: string;
}

export interface HeatCell {
  /** YYYY-MM-DD */
  day: string;
  agent: string;
  count: number;
}

export interface AgentAxes {
  name: string;
  role: string;
  commits: number;
  pullRequests: number;
  reviews: number;
  issues: number;
  messages: number;
}

/** Event kinds, as the Observatory service names them. */
export type ActivityKind =
  | "message"
  | "human"
  | "crossing"
  | "refusal"
  | "commit"
  | "pr_opened"
  | "pr_merged"
  | "pr_closed"
  | "review_approved"
  | "review_changes"
  | "review_comment"
  | "issue_opened"
  | "issue_closed"
  | "comment"
  | "ci_failed"
  | "ci_passed"
  | "review_requested"
  | "assigned"
  | "mentioned"
  | "presence"
  | "task"
  | "tokens";

export interface ActivityEvent {
  id: string;
  at: string;
  actor: string;
  kind: ActivityKind;
  target: string;
  repo: string;
  summary: string;
  url?: string;
}

export interface ThroughputDay {
  /** YYYY-MM-DD */
  day: string;
  prsMerged: Record<string, number>;
  /** Median hours from PR opened to first review that day; null if none. */
  reviewLatencyHours: number | null;
  ciFailures: number;
  tokens: Record<string, number>;
}

export interface Throughput {
  days: ThroughputDay[];
  /** Spend this budget period per agent (not per day; the gateway reports periods). */
  spendUSD: Record<string, number>;
}

export interface Report {
  id: string;
  at: string;
  author: string;
  markdown: string;
}

export interface TimelineFilter {
  kinds?: ActivityKind[];
  actor?: string;
  limit?: number;
}

/** The Observatory Connect service, as the UI uses it. */
export interface ObservatoryApi {
  edges(workspace: string, window: ObsWindow): Promise<Edges>;
  pairMessages(workspace: string, a: string, b: string, window: ObsWindow): Promise<MessageMeta[]>;
  now(workspace: string): Promise<NowRow[]>;
  heatmap(workspace: string, days: number): Promise<HeatCell[]>;
  axes(workspace: string, window: ObsWindow): Promise<AgentAxes[]>;
  timeline(workspace: string, filter: TimelineFilter): Promise<ActivityEvent[]>;
  watchTimeline(workspace: string, signal: AbortSignal): AsyncIterable<ActivityEvent>;
  throughput(workspace: string, days: number): Promise<Throughput>;
  latestReport(workspace: string): Promise<Report | null>;
  reports(workspace: string, limit: number): Promise<Report[]>;
  requestReport(workspace: string): Promise<void>;
}
