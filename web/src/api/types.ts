// Domain types the UI works with. They mirror the control center's Connect
// API (proto/swarm/controlcenter/v1) and the CRDs (api/v1alpha1), flattened to
// what the pages need. connect.ts maps generated messages onto them; mock.ts
// implements them in memory.

export type Role = "admin" | "operator" | "viewer";

export interface Me {
  handle: string;
  email: string;
  displayName: string;
  role: Role;
  /** Workspaces the person may see (all, for admins). */
  workspaces: string[];
}

export interface SetupState {
  adminClaimed: boolean;
  /** PostgreSQL is connected; before that the control center keeps no users. */
  databaseReady: boolean;
  /** SHA-256 fingerprint of the platform CA, to compare with the welcome output. */
  caFingerprint: string;
  url: string;
}

export interface AuthProvider {
  kind: "oidc" | "saml";
  name: string;
  displayName: string;
  /** Where the browser goes to sign in; the UI appends ?return=/path. */
  loginURL: string;
}

export type CheckResult = "pass" | "warn" | "fail" | "pending";

export interface PreflightCheck {
  name: string;
  result: CheckResult;
  message: string;
  fix: string;
}

export type IsolationTier = "auto" | "userns" | "sysbox" | "privileged";

export interface Preflight {
  checks: PreflightCheck[];
  tier: IsolationTier | "";
  roomEstimate: number;
  clusterCIDRs: string[];
  completed: boolean;
}

export interface Condition {
  type: string;
  status: "True" | "False" | "Unknown";
  reason: string;
  message: string;
}

export interface OIDCProvider {
  name: string;
  displayName: string;
  issuer: string;
  clientID: string;
  groupsClaim: string;
  scopes: string[];
  /** Read-only: a client secret is stored. */
  clientSecretSet: boolean;
  /** Read-only: the redirect URL to register at the provider. */
  redirectURL: string;
}

export interface SAMLProvider {
  name: string;
  displayName: string;
  metadataURL: string;
  metadataXML: string;
  groupsAttribute: string;
  /** Read-only: our SP metadata and ACS URLs, for the IdP. */
  spMetadataURL: string;
  acsURL: string;
}

export interface RoleMapping {
  group: string;
  role: Role;
}

export type ProviderKind = "anthropic" | "openai" | "openaiCompatible";

export interface ModelProvider {
  name: string;
  kind: ProviderKind;
  baseURL: string;
  models: string[];
  /** True when a key is stored; the key itself never comes back. */
  hasKey: boolean;
}

export interface PlatformServices {
  postgresStorage: string;
  forgejoStorage: string;
  forgejoOIDCLogin: boolean;
  messagingReplicas: number;
  messagingStorage: string;
  registryCacheStorage: string;
  registryUpstreams: string[];
  ntfyEnabled: boolean;
  ntfyStorage: string;
  providers: ModelProvider[];
}

export type WizardStep = "preflight" | "signin" | "services" | "hostname" | "workspace" | "done";
export const wizardSteps: WizardStep[] = ["preflight", "signin", "services", "hostname", "workspace"];

export interface PlatformSpec {
  hostname: string;
  tlsMode: "selfSigned" | "certManager";
  issuerName: string;
  issuerKind: string;
  isolationTier: IsolationTier;
  privilegedAccepted: boolean;
  localDisabled: boolean;
  oidc: OIDCProvider[];
  saml: SAMLProvider[];
  roleMappings: RoleMapping[];
  defaultRole: Role | "";
  services: PlatformServices;
  wizardStep: WizardStep;
  servicesApproved: boolean;
}

export interface Platform {
  spec: PlatformSpec;
  adminClaimed: boolean;
  url: string;
  caFingerprint: string;
  preflight: Preflight;
  conditions: Condition[];
  versions: Record<string, string>;
}

/** Secret values entered in the wizard; stored in Secrets, never in specs. */
export interface PlatformSecrets {
  providerKeys?: Record<string, string>;
  oidcClientSecrets?: Record<string, string>;
}

// Workspaces

export type CLI = "claude" | "ore";
export type LinkMode = "open" | "mediated" | "none";

export interface RoleTemplate {
  name: string;
  title: string;
  summary: string;
}

export interface RoomDraft {
  name: string;
  role: string;
  cli: CLI;
  model: string;
  effort: string;
  group: string;
  cpu: number;
  memoryGi: number;
  homeGi: number;
  engineGi: number;
  repos: string[];
}

export interface GroupLink {
  from: string;
  to: string;
  mode: LinkMode;
  senders: string[];
  recipients: string[];
  requirePrefix: string;
}

export interface RepositoryDraft {
  name: string;
  importURL: string;
  /** Used once for the import; never stored in the spec. */
  importCredential: string;
  group: string;
  pushMirror: string;
  mirrorToken: string;
}

export type ChannelKind = "webPush" | "ntfy" | "slack" | "discord" | "email";
export type NotificationEvent =
  | "approvalWaiting"
  | "idleWithOpenWork"
  | "ciRed"
  | "budget"
  | "diskLow"
  | "roomCrashed";
export const notificationEvents: NotificationEvent[] = [
  "approvalWaiting",
  "idleWithOpenWork",
  "ciRed",
  "budget",
  "diskLow",
  "roomCrashed",
];

export interface ChannelDraft {
  kind: ChannelKind;
  /** ntfy topic, or webhook URL / SMTP URL for the others (stored in a Secret). */
  target: string;
}

export interface WorkspaceDraft {
  name: string;
  displayName: string;
  org: string;
  defaultModel: string;
  perRoomMonthlyUSD: number;
  rooms: RoomDraft[];
  groups: string[];
  defaultLinks: "withinGroup" | "none";
  links: GroupLink[];
  repositories: RepositoryDraft[];
  channels: ChannelDraft[];
  events: NotificationEvent[];
  /** The server's plan this draft came from, so fields the wizard doesn't edit survive a re-apply. */
  source?: unknown;
}

export interface PlanChange {
  op: "create" | "update" | "delete" | "unchanged";
  kind: "Workspace" | "Room" | "RoleTemplate" | "RunnerPool";
  name: string;
  /** Human-readable field changes, e.g. "model: a -> b". */
  details: string[];
}

export interface PlanResult {
  changes: PlanChange[];
  /** Validation problems that would refuse the apply. */
  problems: string[];
  /** Whether the rooms fit the cluster, per the preflight estimate. */
  fits: boolean;
  fitNote: string;
}

export interface WorkspaceSummary {
  name: string;
  displayName: string;
  rooms: number;
  ready: boolean;
}

export interface RoomProgress {
  name: string;
  phase: RoomPhase;
  conditions: Condition[];
}

// Fleet and rooms

export type Presence = "working" | "waitingOnHuman" | "idle" | "offline" | "crashed";
export type RoomPhase = "Provisioning" | "Running" | "Paused" | "Updating" | "Failed";

export interface RoomSummary {
  workspace: string;
  name: string;
  displayName: string;
  role: string;
  group: string;
  presence: Presence;
  cli: CLI;
  model: string;
  currentTask: string;
  spendUSD: number;
  budgetUSD: number;
  diskUsedBytes: number;
  diskCapacityBytes: number;
  phase: RoomPhase;
  paused: boolean;
  updateAvailable: boolean;
  pendingApprovals: number;
}

export type FleetEvent =
  | { kind: "snapshot"; rooms: RoomSummary[] }
  | { kind: "upsert"; room: RoomSummary }
  | { kind: "remove"; workspace: string; name: string };

export interface PresenceChange {
  presence: Presence;
  at: string;
}

export interface ChatMessage {
  id: string;
  from: string;
  to: string;
  body: string;
  at: string;
  fromKind: "agent" | "human";
}

export interface RoomDetail {
  summary: RoomSummary;
  conditions: Condition[];
  imageVersion: string;
  forgeUser: string;
  repos: string[];
  presenceHistory: PresenceChange[];
  recentMessages: ChatMessage[];
  inputTokens: number;
  outputTokens: number;
  contextTokens: number;
}

export interface TranscriptEntry {
  id: string;
  at: string;
  kind: "user" | "assistant" | "tool" | "toolResult" | "system";
  text: string;
}

export type LogComponent = "supervisor" | "cli" | "engine" | "janitor";

export interface LogLine {
  text: string;
}

export interface Process {
  pid: number;
  user: string;
  cpuPercent: number;
  memoryBytes: number;
  command: string;
}

export interface Resources {
  cpuMillis: number;
  cpuLimitMillis: number;
  memoryBytes: number;
  memoryLimitBytes: number;
  /** False when metrics-server is missing. */
  metricsAvailable: boolean;
  netRxBytesPerSec?: number;
  netTxBytesPerSec?: number;
  processes?: Process[];
}

export interface Volume {
  name: string;
  usedBytes: number;
  capacityBytes: number;
}

export interface EngineItem {
  id: string;
  name: string;
  sizeBytes: number;
  detail: string;
}

export interface Storage {
  /** Undefined when unknown. */
  quotaEnforced?: boolean;
  volumes: Volume[];
  /** Engine contents, when the backend reports them. */
  images?: EngineItem[];
  containers?: EngineItem[];
  engineVolumes?: EngineItem[];
  sharedDirs: { name: string; readOnly: boolean }[];
}

export type PruneKind = "containers" | "images" | "volumes" | "buildCache" | "all";

export type RoomAction =
  | { kind: "pause" }
  | { kind: "resume" }
  | { kind: "recreate"; deleteHome: boolean; confirmName?: string }
  | { kind: "remove"; deleteDataNow: boolean }
  | { kind: "rotate" }
  | { kind: "updateNow" };

// Approvals and inbox

export type Decision = "allow" | "allowForSession" | "deny";

export interface Approval {
  id: string;
  workspace: string;
  room: string;
  cli: CLI;
  tool: string;
  summary: string;
  detail: string;
  reason: string;
  matchingRules: string[];
  sessionScopeAvailable: boolean;
  requestedAt: string;
  answer?: { decision: Decision; message: string; answeredBy: string; at: string };
  withdrawn?: string;
}

export type ApprovalEvent =
  | { kind: "requested"; approval: Approval }
  | { kind: "answered"; id: string; answer: NonNullable<Approval["answer"]> }
  | { kind: "withdrawn"; id: string; reason: string };

export interface Conversation {
  /** The agent's address: name@workspace. */
  address: string;
  displayName: string;
  presence: Presence;
  lastMessage?: ChatMessage;
  unread: number;
}

// Audit, spend, users, notifications

export interface AuditEntry {
  id: string;
  source: "controlCenter" | "messaging";
  at: string;
  actor: string;
  action: string;
  object: string;
  workspace: string;
  sourceIP: string;
  result: string;
  detail: string;
}

export interface AuditFilter {
  actor?: string;
  action?: string;
  workspace?: string;
  since?: string;
  until?: string;
  limit?: number;
}

export interface RoomSpend {
  workspace: string;
  room: string;
  usd: number;
  budgetUSD: number;
  exhausted: boolean;
}

export interface SpendReport {
  /** False until the gateway is reachable. */
  available: boolean;
  updatedAt: string;
  rooms: RoomSpend[];
  workspaces: { workspace: string; usd: number; models: { model: string; usd: number }[] }[];
}

export interface User {
  id: string;
  handle: string;
  email: string;
  displayName: string;
  role: Role;
  workspaces: string[];
  /** "local", "oidc:<name>", "saml:<name>" or "setup". */
  provider: string;
  disabled: boolean;
  /** The role was set by an admin, not by identity-provider groups. */
  roleOverride: boolean;
  lastSeen: string;
}

export interface NotificationPref {
  /** "" means every workspace. */
  workspace: string;
  event: NotificationEvent;
  /** Empty: off. */
  channels: ChannelKind[];
}

export interface NotificationPrefs {
  prefs: NotificationPref[];
  /** Channels the person's workspaces have configured. */
  availableChannels: ChannelKind[];
}
