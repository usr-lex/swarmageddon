import type {
  Approval,
  ApprovalEvent,
  AuditEntry,
  AuditFilter,
  AuthProvider,
  Capacity,
  ChatMessage,
  Conversation,
  Decision,
  FleetEvent,
  LogLine,
  Me,
  NotificationPrefs,
  Platform,
  PlatformSecrets,
  PlatformSpec,
  PlanChange,
  PruneKind,
  Resources,
  RoleTemplate,
  RoomAction,
  RoomDetail,
  RoomProgress,
  SetupState,
  SpendRow,
  Storage,
  TranscriptEntry,
  User,
  Role,
  WorkspaceDraft,
  WorkspaceSummary,
} from "./types";
import type { ObservatoryApi } from "./observatory";

/** Error with a Connect-like code, so pages can react to "unauthenticated". */
export class ApiError extends Error {
  constructor(
    public code: "unauthenticated" | "permission_denied" | "invalid_argument" | "not_found" | "resource_exhausted" | "unknown",
    message: string,
  ) {
    super(message);
  }
}

/**
 * Everything the UI asks of the control center. Streams are async iterables
 * that end when the signal aborts.
 */
export interface Api {
  /** The workspace home page (§8.7). */
  observatory: ObservatoryApi;

  // Setup (§3.3)
  getSetupState(): Promise<SetupState>;
  /** Checks the setup token; returns a short-lived claim the next call needs. */
  claimSetup(token: string): Promise<{ claim: string }>;
  createLocalAdmin(claim: string, admin: { username: string; email: string; password: string }): Promise<void>;
  /** Skip the local admin and configure an IdP first (the claim keeps the wizard's sign-in step open). */
  continueWithIdP(claim: string): Promise<void>;

  // Auth (§8.1)
  me(): Promise<Me>;
  listAuthProviders(): Promise<AuthProvider[]>;
  loginLocal(username: string, password: string): Promise<void>;
  logout(): Promise<void>;

  // Platform (§3.4)
  getPlatform(): Promise<Platform>;
  watchPlatform(signal: AbortSignal): AsyncIterable<Platform>;
  updatePlatform(spec: PlatformSpec, secrets?: PlatformSecrets): Promise<Platform>;
  rerunPreflight(): Promise<void>;

  // Workspaces (§8.3)
  listWorkspaces(): Promise<WorkspaceSummary[]>;
  getWorkspaceDraft(name: string): Promise<WorkspaceDraft>;
  listRoleTemplates(): Promise<RoleTemplate[]>;
  listModels(): Promise<string[]>;
  /** Room names used anywhere on the platform. */
  usedRoomNames(): Promise<string[]>;
  capacity(): Promise<Capacity>;
  planWorkspace(draft: WorkspaceDraft): Promise<PlanChange[]>;
  applyWorkspace(draft: WorkspaceDraft): Promise<void>;
  watchProvisioning(workspace: string, signal: AbortSignal): AsyncIterable<RoomProgress[]>;

  // Fleet and rooms (§8.4)
  watchFleet(signal: AbortSignal): AsyncIterable<FleetEvent>;
  getRoom(workspace: string, room: string): Promise<RoomDetail>;
  watchTranscript(workspace: string, room: string, signal: AbortSignal): AsyncIterable<TranscriptEntry>;
  watchLogs(workspace: string, room: string, signal: AbortSignal): AsyncIterable<LogLine>;
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

  // Audit, spend, users, notifications
  queryAudit(filter: AuditFilter): Promise<AuditEntry[]>;
  getSpend(period: "month" | "week" | "day"): Promise<SpendRow[]>;
  listUsers(): Promise<User[]>;
  updateUser(handle: string, role: Role, workspaces: string[]): Promise<void>;
  getVapidPublicKey(): Promise<string>;
  subscribePush(sub: PushSubscriptionJSON): Promise<void>;
  unsubscribePush(endpoint: string): Promise<void>;
  getNotificationPrefs(): Promise<NotificationPrefs>;
  setNotificationPrefs(p: NotificationPrefs): Promise<void>;
}
