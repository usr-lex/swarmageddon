import type {
  Approval,
  ApprovalEvent,
  AuditEntry,
  AuditFilter,
  AuthProvider,
  ChatMessage,
  Conversation,
  Decision,
  FleetEvent,
  IsolationTier,
  LogComponent,
  LogLine,
  Me,
  NotificationPrefs,
  Platform,
  PlatformSecrets,
  PlatformSpec,
  PlanResult,
  PruneKind,
  Resources,
  Role,
  RoleTemplate,
  RoomAction,
  RoomDetail,
  RoomProgress,
  SetupState,
  SpendReport,
  Storage,
  TranscriptEntry,
  User,
  WizardStep,
  WorkspaceDraft,
  WorkspaceSummary,
} from "./types";
import type { ObservatoryApi } from "./observatory";
import type { ChannelInfo, DeliveryMode, InboxItem, MentionCandidate, ReadPage, ReadScope, Verdict } from "./channels";

/** Error with a Connect-like code, so pages can react to "unauthenticated". */
export class ApiError extends Error {
  constructor(
    public code: "unauthenticated" | "permission_denied" | "invalid_argument" | "not_found" | "resource_exhausted" | "unimplemented" | "unknown",
    message: string,
  ) {
    super(message);
  }
}

/**
 * Everything the UI asks of the control center (proto/swarm/controlcenter/v1).
 * Streams are async iterables that end when the signal aborts.
 */
export interface Api {
  /** The workspace home page (§8.7). */
  observatory: ObservatoryApi;

  // Setup (§3.3)
  getSetupState(): Promise<SetupState>;
  /**
   * Checks the setup token and creates the local admin; with admin null, signs
   * the browser in as a temporary admin to set up OIDC or SAML first.
   */
  claimAdmin(token: string, admin: { username: string; email: string; password: string } | null): Promise<void>;

  // Auth (§8.1)
  me(): Promise<Me>;
  listAuthProviders(): Promise<AuthProvider[]>;
  loginLocal(username: string, password: string): Promise<void>;
  logout(): Promise<void>;

  // Platform (§3.4). Each update sends only its part of the spec.
  getPlatform(): Promise<Platform>;
  watchPlatform(signal: AbortSignal): AsyncIterable<Platform>;
  updateIsolation(tier: IsolationTier, privilegedAccepted: boolean): Promise<Platform>;
  updateSignIn(spec: PlatformSpec, secrets?: PlatformSecrets): Promise<Platform>;
  updateServices(spec: PlatformSpec, secrets?: PlatformSecrets): Promise<Platform>;
  updateHostname(spec: PlatformSpec): Promise<Platform>;
  setWizardStep(step: WizardStep): Promise<void>;
  rerunPreflight(): Promise<void>;

  // Workspaces (§8.3)
  listWorkspaces(): Promise<WorkspaceSummary[]>;
  getWorkspaceDraft(name: string): Promise<WorkspaceDraft>;
  listRoleTemplates(): Promise<RoleTemplate[]>;
  listModels(): Promise<string[]>;
  /** Names from the default pool not used anywhere, and the default role mix for that many. */
  suggestAgentNames(count: number, exclude: string[]): Promise<{ names: string[]; roles: string[] }>;
  planWorkspace(draft: WorkspaceDraft): Promise<PlanResult>;
  applyWorkspace(draft: WorkspaceDraft): Promise<void>;
  watchProvisioning(workspace: string, signal: AbortSignal): AsyncIterable<RoomProgress[]>;

  // Fleet and rooms (§8.4)
  watchFleet(signal: AbortSignal): AsyncIterable<FleetEvent>;
  getRoom(workspace: string, room: string): Promise<RoomDetail>;
  watchTranscript(workspace: string, room: string, signal: AbortSignal): AsyncIterable<TranscriptEntry>;
  watchLogs(workspace: string, room: string, component: LogComponent | "", signal: AbortSignal): AsyncIterable<LogLine>;
  getResources(workspace: string, room: string): Promise<Resources>;
  getStorage(workspace: string, room: string): Promise<Storage>;
  prune(workspace: string, room: string, kind: PruneKind): Promise<{ reclaimedBytes: number }>;
  roomAction(workspace: string, room: string, action: RoomAction): Promise<void>;

  // Approvals (§8.5)
  listApprovals(): Promise<Approval[]>;
  watchApprovals(signal: AbortSignal): AsyncIterable<ApprovalEvent>;
  getApproval(id: string): Promise<Approval>;
  answerApproval(id: string, decision: Decision, message: string): Promise<void>;

  // Inbox
  listConversations(): Promise<Conversation[]>;
  listMessages(address: string): Promise<ChatMessage[]>;
  watchMessages(signal: AbortSignal): AsyncIterable<ChatMessage>;
  sendMessage(to: string, body: string): Promise<ChatMessage>;

  // Channels (§7.8): the person reads and posts like any member.
  listChannels(): Promise<ChannelInfo[]>;
  readMessages(workspace: string, scope: ReadScope, opts: { unreadOnly: boolean; limit: number; cursor?: string }): Promise<ReadPage>;
  post(workspace: string, channel: string, body: string): Promise<{ id: string; mentioned: string[] }>;
  setDelivery(workspace: string, channel: string, mode: DeliveryMode): Promise<void>;
  markRead(workspace: string, scope: ReadScope, upTo?: string): Promise<void>;
  watchItems(signal: AbortSignal): AsyncIterable<InboxItem>;
  mentionCandidates(workspace: string): Promise<MentionCandidate[]>;

  // Warden (ADR 0011)
  listVerdicts(workspace: string, room?: string): Promise<Verdict[]>;
  decideVerdict(id: string, decision: "confirmed" | "dismissed"): Promise<void>;

  // Audit, spend, users, notifications
  queryAudit(filter: AuditFilter): Promise<AuditEntry[]>;
  getSpend(): Promise<SpendReport>;
  listUsers(): Promise<User[]>;
  updateUser(id: string, change: { role?: Role; workspaces?: string[]; disabled?: boolean }): Promise<void>;
  getVapidPublicKey(): Promise<string>;
  subscribePush(sub: PushSubscriptionJSON): Promise<void>;
  unsubscribePush(endpoint: string): Promise<void>;
  getNotificationPrefs(): Promise<NotificationPrefs>;
  setNotificationPrefs(p: NotificationPrefs): Promise<void>;
  sendTestNotification(channel: string): Promise<void>;
}
