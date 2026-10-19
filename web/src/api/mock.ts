// In-memory control center for development, Vitest and Playwright.
// Enabled with ?mock=1 (fresh install) or ?mock=demo (a running platform).
// State survives reloads in sessionStorage, so deep links work in e2e.

import { ApiError, type Api } from "./api";
import { Hub } from "./stream";
import type {
  Approval,
  ApprovalEvent,
  AuditEntry,
  ChatMessage,
  Condition,
  FleetEvent,
  Me,
  NotificationPrefs,
  Platform,
  PlatformSpec,
  PlanChange,
  RoomDraft,
  RoomProgress,
  RoomSummary,
  TranscriptEntry,
  User,
  WorkspaceDraft,
} from "./types";
import { defaultChatChannels, defaultRoles, defaultRoom, defaultStorm, defaultWarden, pickNames, validateDraft } from "@/lib/workspace";
import { effectiveFor, emptyMcp, emptySettings, secretKeyPattern, validateSettings, type RoomSettings } from "./settings";
import { createMockObservatory, demoLinks, demoSwarm } from "./mockObservatory";
import { demoPosts, demoVerdicts } from "./mockSeeds";
import { topSeverity, type ChannelAccess, type ChannelInfo, type ChatChannelDraft, type DeliveryMode, type InboxItem, type Verdict } from "./channels";

export const MOCK_SETUP_TOKEN = "MOCKTOKENMOCKTOKENMOCKTOKENMOCKTO";
const STORE_KEY = "swarm-mock-state";
const GiB = 1024 ** 3;

interface State {
  adminClaimed: boolean;
  admin?: { username: string; email: string; password: string };
  me: Me | null;
  platform: Platform;
  serviceTicks: number;
  workspaces: Record<string, WorkspaceDraft>;
  rooms: (RoomSummary & { ticks: number })[];
  approvals: Approval[];
  messages: ChatMessage[];
  audit: AuditEntry[];
  users: User[];
  prefs: NotificationPrefs;
  pushSubs: string[];
  posts: InboxItem[];
  readIds: string[];
  delivery: Record<string, DeliveryMode>;
  verdicts: Verdict[];
  /** room-secrets per workspace: key -> value. */
  roomSecrets: Record<string, Record<string, string>>;
}

const serviceConditions = [
  "PostgresReady",
  "ForgejoReady",
  "MessagingReady",
  "GatewayReady",
  "RegistryCacheReady",
  "NtfyReady",
];

function now() {
  return new Date().toISOString();
}

let seq = 0;
function id(prefix: string) {
  seq++;
  return `${prefix}${Date.now().toString(36)}${seq.toString(36)}`.toUpperCase();
}

function freshPlatform(): Platform {
  const spec: PlatformSpec = {
    hostname: "",
    tlsMode: "selfSigned",
    issuerName: "",
    issuerKind: "",
    isolationTier: "auto",
    privilegedAccepted: false,
    localDisabled: false,
    oidc: [],
    saml: [],
    roleMappings: [],
    defaultRole: "viewer",
    services: {
      postgresStorage: "20Gi",
      forgejoStorage: "50Gi",
      forgejoOIDCLogin: false,
      messagingReplicas: 1,
      messagingStorage: "10Gi",
      registryCacheStorage: "100Gi",
      registryUpstreams: ["docker.io"],
      ntfyEnabled: false,
      ntfyStorage: "5Gi",
      providers: [],
    },
    wizardStep: "preflight",
    servicesApproved: false,
  };
  return {
    spec,
    adminClaimed: false,
    url: "https://192.168.1.20/",
    caFingerprint: "SHA256:5F:3A:9C:12:E8:71:0B:44:D2:6E:AA:91:7C:03:B8:55:1D:E0:42:7F:96:C3:28:6A:0F:B1:4D:E9:83:72:5C:10",
    preflight: {
      checks: [
        { name: "kubernetesVersion", result: "pass", message: "v1.36.1+k3s1", fix: "" },
        { name: "architecture", result: "pass", message: "amd64 (1 node)", fix: "" },
        { name: "defaultStorageClass", result: "pass", message: "local-path (default)", fix: "" },
        {
          name: "volumeSharing",
          result: "warn",
          message: "local-path does not support ReadWriteMany.",
          fix: "Shared image store and shared folders stay off. Install an RWX provisioner (e.g. Longhorn) to enable them.",
        },
        {
          name: "volumeSizeEnforcement",
          result: "warn",
          message: "local-path does not enforce sizes.",
          fix: "Quotas are soft: the janitor and warnings enforce them. Use TopoLVM, OpenEBS LVM/ZFS, Longhorn or XFS project quotas for hard limits.",
        },
        { name: "idmapVolumes", result: "pass", message: "hostUsers: false pod wrote to a fresh claim.", fix: "" },
        { name: "networkPolicy", result: "pass", message: "Denied connection was refused.", fix: "" },
        { name: "isolationTier", result: "pass", message: "userns works; privileged pods are admitted.", fix: "" },
        { name: "capacity", result: "pass", message: "15.2 CPU, 58 GiB memory, 820 GiB disk free.", fix: "" },
        {
          name: "ingress",
          result: "warn",
          message: "Default IngressClass traefik; no hostname set.",
          fix: "Optional: set a hostname in the Hostname & TLS step and point DNS at the node.",
        },
        { name: "clusterCIDRs", result: "pass", message: "10.42.0.0/16, 10.43.0.0/16", fix: "" },
      ],
      tier: "userns",
      roomEstimate: 7,
      clusterCIDRs: ["10.42.0.0/16", "10.43.0.0/16"],
      completed: true,
    },
    conditions: [{ type: "PreflightDone", status: "True", reason: "Completed", message: "" }],
    versions: { operator: "0.1.0", roomImage: "0.1.0", claudeCode: "2.4.1", ore: "1.161.0", forgejo: "16.0.2" },
  };
}

function freshState(): State {
  return {
    adminClaimed: false,
    me: null,
    platform: freshPlatform(),
    serviceTicks: 0,
    workspaces: {},
    rooms: [],
    approvals: [],
    messages: [],
    audit: [],
    users: [],
    prefs: { prefs: [], availableChannels: ["webPush"] },
    pushSubs: [],
    posts: [],
    readIds: [],
    delivery: {},
    verdicts: [],
    roomSecrets: {},
  };
}

function demoState(): State {
  const s = freshState();
  s.adminClaimed = true;
  s.platform.adminClaimed = true;
  s.platform.preflight.checks = s.platform.preflight.checks.map((c) =>
    c.name === "volumeSharing" ? { ...c, result: "pass", message: "a ReadWriteMany claim is shared between pods", fix: "" } : c,
  );
  s.admin = { username: "admin", email: "admin@example.com", password: "admin" };
  s.me = { handle: "admin", email: "admin@example.com", displayName: "Admin", role: "admin", workspaces: ["acme"] };
  s.platform.spec.wizardStep = "done";
  s.platform.spec.servicesApproved = true;
  s.platform.spec.services.providers = [
    { name: "anthropic", kind: "anthropic", baseURL: "", models: ["claude-opus-5-5", "claude-sonnet-5"], hasKey: true },
  ];
  s.platform.conditions.push(
    ...serviceConditions.map((t) => ({ type: t, status: "True" as const, reason: "Ready", message: "" })),
  );
  const draft: WorkspaceDraft = {
    name: "acme",
    displayName: "Acme",
    org: "acme",
    defaultModel: "claude-opus-5-5",
    perRoomMonthlyUSD: 200,
    rooms: demoSwarm.map((a, i) => ({ ...defaultRoom(a.name, a.role, "claude-opus-5-5"), group: a.group, cli: i % 3 === 2 ? "ore" : "claude" })),
    groups: ["clean", "dirty"],
    defaultLinks: "withinGroup",
    links: demoLinks.map((l) => ({ ...l, mode: "mediated" as const, requirePrefix: l.from === "dirty" ? "to {recipient}:" : "" })),
    repositories: [{ name: "api", importURL: "", importCredential: "", group: "clean", pushMirror: "", mirrorToken: "" }],
    channels: [{ kind: "webPush", target: "" }],
    events: ["approvalWaiting", "roomCrashed", "wardenFlag"],
    chatChannels: [],
    storm: defaultStorm(),
    warden: defaultWarden(),
    advisors: {},
    roomDefaults: { env: { TZ: "UTC" }, secretEnv: [], mcpServers: {}, network: { mode: "limited", domains: ["api.github.com"], packageManagers: true } },
    roleDefaults: { reviewer: { env: {}, secretEnv: [], mcpServers: { linear: { ...emptyMcp(), type: "http", url: "https://mcp.linear.app/mcp" } } } },
    sharedVolumes: [{ name: "datasets", sizeGi: 20, storageClass: "", mounts: [{ select: "everyone", readOnly: true }, { select: "role:implementer", readOnly: false }] }],
  };
  draft.chatChannels = defaultChatChannels(draft);
  draft.warden = { ...defaultWarden(), model: "claude-haiku-5" };
  draft.advisors = { implementer: { model: "claude-opus-5-5", effort: "max", maxCallsPerHour: 10, monthlyUSD: 0, native: false } };
  s.workspaces.acme = draft;
  const demo: Record<string, [RoomSummary["presence"], string, number, number]> = {
    athena: ["working", "Splitting the billing epic into task cards", 42.1, 12],
    hermes: ["waitingOnHuman", "Waiting for approval: npm install", 61.5, 18],
    proteus: ["working", "Port invoice renderer to streaming", 55.0, 22],
    apollo: ["working", "Reviewing #142 (orders pagination)", 30.4, 9],
    artemis: ["idle", "", 12.2, 14],
    iris: ["idle", "", 4.1, 3],
    metis: ["waitingOnHuman", "Waiting for approval: patch src/server.go", 22.8, 10],
    hecate: ["working", "Dedup crawled pages", 48.9, 31],
    hephaestus: ["crashed", "CLI exited 3 times", 7.7, 70],
    nyx: ["offline", "", 0, 2],
  };
  s.rooms = draft.rooms.map((r) => {
    const [presence, currentTask, spendUSD, diskGi] = demo[r.name];
    return {
      ...summaryFromDraft("acme", r, 200),
      presence,
      phase: presence === "offline" ? "Paused" : "Running",
      paused: presence === "offline",
      currentTask,
      spendUSD,
      diskUsedBytes: diskGi * GiB,
      pendingApprovals: presence === "waitingOnHuman" ? 1 : 0,
      ticks: 99,
    };
  });
  s.rooms[3].updateAvailable = true;
  s.posts = demoPosts();
  s.readIds = s.posts.slice(0, 6).map((p) => p.id);
  s.verdicts = demoVerdicts();
  const heph = s.rooms.find((r) => r.name === "hephaestus")!;
  heph.quarantined = true;
  heph.quarantineReason = "Gateway guard: upload of ~/.ssh to an unknown host (critical)";
  for (const r of s.rooms) r.verdict = topSeverity(s.verdicts.filter((v) => v.room === r.name));
  s.approvals = [
    {
      id: "01JDEMOAPPROVAL0000000001",
      workspace: "acme",
      room: "hermes",
      cli: "claude",
      tool: "Bash",
      summary: "npm install left-pad@1.3.0",
      detail: '{"command":"npm install left-pad@1.3.0","description":"Install dependency"}',
      reason: "The task card asks for padded order numbers.",
      matchingRules: ['allow: Bash(npm test)', 'ask: Bash(npm install:*)'],
      sessionScopeAvailable: true,
      requestedAt: now(),
    },
    {
      id: "01JDEMOAPPROVAL0000000002",
      workspace: "acme",
      room: "metis",
      cli: "ore",
      tool: "patch",
      summary: "Edit src/server.go (+3 -1)",
      detail:
        "--- a/src/server.go\n+++ b/src/server.go\n@@ -10,7 +10,9 @@\n func main() {\n-\tlisten(\":8080\")\n+\taddr := os.Getenv(\"ADDR\")\n+\tif addr == \"\" { addr = \":8080\" }\n+\tlisten(addr)\n }\n",
      reason: "",
      matchingRules: ["approval_policy: on-request"],
      sessionScopeAvailable: true,
      requestedAt: now(),
    },
  ];
  s.messages = [
    { id: id("M"), from: "athena@acme", to: "admin@acme", body: "Pagination is done; PR #12 is up for review.", at: now(), fromKind: "agent" },
    { id: id("M"), from: "admin@acme", to: "athena@acme", body: "Thanks. Please also add a test for the last page.", at: now(), fromKind: "human" },
    { id: id("M"), from: "hermes@acme", to: "admin@acme", body: "Can I add left-pad? See the approval.", at: now(), fromKind: "agent" },
  ];
  s.audit = [
    { id: id("A"), source: "controlCenter", at: now(), actor: "admin", action: "platform.update", object: "Platform/default", workspace: "", sourceIP: "192.168.1.5", result: "ok", detail: "wizard.step: services -> hostname" },
    { id: id("A"), source: "controlCenter", at: now(), actor: "admin", action: "workspace.apply", object: "Workspace/acme", workspace: "acme", sourceIP: "192.168.1.5", result: "ok", detail: "10 rooms created" },
    { id: id("A"), source: "controlCenter", at: now(), actor: "alice", action: "terminal.open", object: "Room/acme/athena", workspace: "acme", sourceIP: "10.0.0.9", result: "ok", detail: '{"mode":"shell"}' },
    { id: id("A"), source: "messaging", at: now(), actor: "hermes@acme", action: "crossing", object: "metis@acme", workspace: "acme", sourceIP: "", result: "ok", detail: "clean->dirty" },
  ];
  s.users = [
    { id: "u1", handle: "admin", email: "admin@example.com", displayName: "Admin", role: "admin", workspaces: [], provider: "local", disabled: false, roleOverride: false, lastSeen: now() },
    { id: "u2", handle: "alice", email: "alice@example.com", displayName: "Alice", role: "operator", workspaces: ["acme"], provider: "oidc:keycloak", disabled: false, roleOverride: false, lastSeen: now() },
    { id: "u3", handle: "bob", email: "bob@example.com", displayName: "Bob", role: "viewer", workspaces: ["acme"], provider: "saml:corp", disabled: false, roleOverride: true, lastSeen: now() },
  ];
  return s;
}

function summaryFromDraft(ws: string, r: RoomDraft, budget: number): RoomSummary {
  return {
    workspace: ws,
    name: r.name,
    displayName: r.name[0].toUpperCase() + r.name.slice(1),
    role: r.role,
    group: r.group,
    presence: "offline",
    cli: r.cli,
    model: r.model,
    currentTask: "",
    spendUSD: 0,
    budgetUSD: budget,
    diskUsedBytes: 0,
    diskCapacityBytes: (r.homeGi + r.engineGi) * GiB,
    phase: "Provisioning",
    paused: false,
    updateAvailable: false,
    pendingApprovals: 0,
  };
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

/** Creates the mock. `mode` is "1" (fresh) or "demo"; state is kept per tab. */
export function createMockApi(mode: string): Api {
  let s: State;
  try {
    const saved = sessionStorage.getItem(STORE_KEY);
    s = saved ? (JSON.parse(saved) as State) : mode === "demo" ? demoState() : freshState();
  } catch {
    s = mode === "demo" ? demoState() : freshState();
  }
  const save = () => {
    try {
      sessionStorage.setItem(STORE_KEY, JSON.stringify(s));
    } catch {
      /* private mode: state lives in memory only */
    }
  };
  save();

  s.roomSecrets ??= {};
  const effectiveOf = (d: WorkspaceDraft) => d.rooms.map((r) => effectiveFor(r.name, r.role, d.roomDefaults, d.roleDefaults?.[r.role], r.settings));
  const draftOf = (ws: string) => {
    const d = s.workspaces[ws];
    if (!d) throw new ApiError("not_found", `Workspace ${ws} not found.`);
    return d;
  };
  const roomDraft = (ws: string, name: string) => {
    const r = draftOf(ws).rooms.find((x) => x.name === name);
    if (!r) throw new ApiError("not_found", `Room ${name} not found.`);
    return r;
  };
  const mountsOf = (d: WorkspaceDraft, r: RoomDraft): Record<string, "ro" | "rw"> => {
    const out: Record<string, "ro" | "rw"> = {};
    for (const v of d.sharedVolumes ?? []) {
      for (const m of v.mounts) {
        const hit = m.select === "everyone" || m.select === `role:${r.role}` || m.select === `room:${r.name}` || (r.group && m.select === `group:${r.group}`);
        if (hit && out[v.name] !== "rw") out[v.name] = m.readOnly ? "ro" : "rw";
      }
    }
    return out;
  };
  const platformHub = new Hub<Platform>();
  const fleetHub = new Hub<FleetEvent>();
  const approvalHub = new Hub<ApprovalEvent>();
  const messageHub = new Hub<ChatMessage>();
  const provisionHub = new Hub<string>();

  const audit = (action: string, object: string, detail = "", workspace = "") =>
    s.audit.unshift({ id: id("A"), source: "controlCenter", at: now(), actor: s.me?.handle ?? "anonymous", action, object, workspace, sourceIP: "127.0.0.1", result: "ok", detail });

  const requireMe = () => {
    if (!s.me) throw new ApiError("unauthenticated", "Sign in first.");
    return s.me;
  };
  const requireRole = (...roles: Me["role"][]) => {
    const me = requireMe();
    if (!roles.includes(me.role)) throw new ApiError("permission_denied", "Your role can't do that.");
  };
  const room = (ws: string, name: string) => {
    const r = s.rooms.find((x) => x.workspace === ws && x.name === name);
    if (!r) throw new ApiError("not_found", `Room ${name}@${ws} not found.`);
    return r;
  };
  const refreshVerdict = (ws: string, name: string) => {
    const r = s.rooms.find((x) => x.workspace === ws && x.name === name);
    if (!r) return;
    r.verdict = topSeverity(s.verdicts.filter((v) => v.workspace === ws && v.room === name));
    upsert(r);
  };
  const itemHub = new Hub<InboxItem>();
  const verdictHub = new Hub<Verdict>();
  const access = (ws: string, ch: ChatChannelDraft): ChannelAccess | null => {
    const handle = s.me?.handle ?? "";
    let best: ChannelAccess | null = null;
    const order = { read: 1, write: 1, readWrite: 2 };
    for (const m of ch.members) {
      if (m.select === "everyone" || m.select === "people" || m.select === `person:${handle}`) {
        if (!best || order[m.access] > order[best]) best = m.access;
      }
    }
    return best ?? (s.me?.role === "admin" && s.workspaces[ws] ? "read" : null);
  };
  const unreadIn = (ws: string, ch: string) => s.posts.filter((p) => p.workspace === ws && p.channel === ch && !s.readIds.includes(p.id) && p.from !== s.me?.handle).length;
  const directItems = (ws: string): InboxItem[] =>
    s.messages
      .filter((m) => m.to.endsWith(`@${ws}`) || m.from.endsWith(`@${ws}`))
      .map((m) => ({ id: m.id, workspace: ws, channel: "", from: m.from, fromKind: m.fromKind, body: m.body, at: m.at, read: m.fromKind === "human" || s.readIds.includes(m.id), mentionsMe: false }));
  const mentions = (body: string) => [...body.matchAll(/@([a-z][a-z0-9-]*(?::[a-z0-9_-]+)?)/g)].map((m) => m[1]);
  const newPost = (ws: string, channel: string, from: string, fromKind: "agent" | "human", body: string): InboxItem => ({
    id: id("P"),
    workspace: ws,
    channel,
    from,
    fromKind,
    body,
    at: now(),
    read: fromKind === "human" && from === s.me?.handle,
    mentionsMe: !!s.me && mentions(body).includes(s.me.handle),
  });

  const strip = ({ ticks: _t, ...r }: RoomSummary & { ticks: number }): RoomSummary => r;
  const upsert = (r: RoomSummary & { ticks: number }) => fleetHub.publish({ kind: "upsert", room: strip(r) });

  // One tick advances everything the operator would be doing.
  const tick = () => {
    let changed = false;
    const p = s.platform;
    if (p.spec.servicesApproved) {
      const wanted = serviceConditions.filter((c) => c !== "NtfyReady" || p.spec.services.ntfyEnabled);
      const next = wanted.find((t) => !p.conditions.some((c) => c.type === t && c.status === "True"));
      if (next) {
        s.serviceTicks++;
        const existing = p.conditions.find((c) => c.type === next);
        if (!existing) {
          p.conditions.push({ type: next, status: "False", reason: "Deploying", message: "Waiting for pods to become ready" });
        } else if (s.serviceTicks % 2 === 0) {
          existing.status = "True";
          existing.reason = "Ready";
          existing.message = "";
        }
        if (!wanted.find((t) => !p.conditions.some((c) => c.type === t && c.status === "True"))) {
          p.conditions.push({ type: "Ready", status: "True", reason: "AllServicesReady", message: "" });
        }
        changed = true;
        platformHub.publish(structuredClone(p));
      }
    }
    for (const r of s.rooms) {
      if (r.phase !== "Provisioning") continue;
      r.ticks++;
      if (r.ticks >= 3) {
        r.phase = "Running";
        r.presence = "idle";
        r.currentTask = "";
      }
      changed = true;
      upsert(r);
      provisionHub.publish(r.workspace);
      if (r.phase === "Running") maybeFirstApproval(r.workspace);
    }
    if (changed) save();
  };

  // Once a new workspace is running, its first agent asks for something, so
  // the approvals flow has data.
  const maybeFirstApproval = (ws: string) => {
    const rooms = s.rooms.filter((r) => r.workspace === ws);
    if (rooms.some((r) => r.phase !== "Running") || s.approvals.some((a) => a.workspace === ws)) return;
    const r = rooms[0];
    const a: Approval = {
      id: id("AP"),
      workspace: ws,
      room: r.name,
      cli: r.cli,
      tool: r.cli === "claude" ? "Bash" : "exec",
      summary: "git push origin feature/setup",
      detail: '{"command":"git push origin feature/setup"}',
      reason: "Push the scaffold for review.",
      matchingRules: ["ask: Bash(git push:*)"],
      sessionScopeAvailable: true,
      requestedAt: now(),
    };
    s.approvals.push(a);
    r.presence = "waitingOnHuman";
    r.currentTask = `Waiting for approval: ${a.summary}`;
    r.pendingApprovals = 1;
    upsert(r);
    approvalHub.publish({ kind: "requested", approval: a });
  };

  setInterval(tick, 250);

  const update = async (spec: PlatformSpec) => {
    requireRole("admin");
    const before = s.platform.spec;
    s.platform.spec = spec;
    audit("platform.update", "Platform/default", before.wizardStep !== spec.wizardStep ? `wizard.step: ${before.wizardStep} -> ${spec.wizardStep}` : "");
    save();
    platformHub.publish(structuredClone(s.platform));
    return structuredClone(s.platform);
  };

  const api: Api = {
    observatory: createMockObservatory(() => s.rooms, () => s.posts),
    async getSetupState() {
      return { adminClaimed: s.adminClaimed, databaseReady: true, caFingerprint: s.platform.caFingerprint, url: s.platform.url };
    },
    async claimAdmin(token, admin) {
      await sleep(50);
      if (token.trim().toUpperCase() !== MOCK_SETUP_TOKEN) throw new ApiError("permission_denied", "That setup token is not right.");
      if (!admin) {
        s.me = { handle: "setup", email: "", displayName: "Setup", role: "admin", workspaces: [] };
        s.platform.spec.wizardStep = "signin";
        save();
        return;
      }
      if (admin.password.length < 12) throw new ApiError("invalid_argument", "Use at least 12 characters.");
      s.admin = admin;
      s.adminClaimed = true;
      s.platform.adminClaimed = true;
      s.me = { handle: admin.username, email: admin.email, displayName: admin.username, role: "admin", workspaces: [] };
      s.users.push({ id: id("U"), ...s.me, workspaces: [], provider: "local", disabled: false, roleOverride: false, lastSeen: now() });
      audit("setup.claim", "Platform/default", "local admin created");
      save();
    },

    async me() {
      return requireMe();
    },
    async listAuthProviders() {
      return [
        ...s.platform.spec.oidc.map((p) => ({ kind: "oidc" as const, name: p.name, displayName: p.displayName || p.name, loginURL: `/auth/oidc/${p.name}/login` })),
        ...s.platform.spec.saml.map((p) => ({ kind: "saml" as const, name: p.name, displayName: p.displayName || p.name, loginURL: `/auth/saml/${p.name}/login` })),
      ];
    },
    async loginLocal(username, password) {
      await sleep(50);
      if (!s.admin || s.admin.username !== username || s.admin.password !== password)
        throw new ApiError("unauthenticated", "Wrong username or password.");
      s.me = { handle: username, email: s.admin.email, displayName: username, role: "admin", workspaces: Object.keys(s.workspaces) };
      save();
    },
    async logout() {
      s.me = null;
      save();
    },

    async getPlatform() {
      requireMe();
      return structuredClone(s.platform);
    },
    async *watchPlatform(signal) {
      requireMe();
      yield* platformHub.subscribe(signal, [structuredClone(s.platform)]);
    },
    async updateIsolation(tier, privilegedAccepted) {
      return update({ ...s.platform.spec, isolationTier: tier, privilegedAccepted });
    },
    async updateSignIn(spec, secrets) {
      const next = structuredClone(spec);
      for (const o of next.oidc) {
        o.redirectURL = `${location.origin}/auth/oidc/${o.name}/callback`;
        if (secrets?.oidcClientSecrets?.[o.name]) o.clientSecretSet = true;
      }
      for (const x of next.saml) {
        x.spMetadataURL = `${location.origin}/auth/saml/${x.name}/metadata`;
        x.acsURL = `${location.origin}/auth/saml/${x.name}/acs`;
      }
      return update({ ...s.platform.spec, oidc: next.oidc, saml: next.saml, roleMappings: spec.roleMappings, defaultRole: spec.defaultRole, localDisabled: spec.localDisabled, services: { ...s.platform.spec.services, forgejoOIDCLogin: spec.services.forgejoOIDCLogin } });
    },
    async updateServices(spec, secrets) {
      const services = structuredClone(spec.services);
      for (const pr of services.providers) if (secrets?.providerKeys?.[pr.name]) pr.hasKey = true;
      return update({ ...s.platform.spec, services, servicesApproved: spec.servicesApproved });
    },
    async updateHostname(spec) {
      const before = s.platform.spec.hostname;
      if (spec.hostname && spec.hostname !== before) {
        s.platform.url = `https://${spec.hostname}/`;
        s.platform.conditions = s.platform.conditions.filter((c) => c.type !== "ControlCenterTLSReady");
        s.platform.conditions.push({ type: "ControlCenterTLSReady", status: "True", reason: "Issued", message: "" });
      }
      return update({ ...s.platform.spec, hostname: spec.hostname, tlsMode: spec.tlsMode, issuerName: spec.issuerName, issuerKind: spec.issuerKind });
    },
    async setWizardStep(step) {
      await update({ ...s.platform.spec, wizardStep: step });
    },
    async rerunPreflight() {
      requireRole("admin");
      s.platform.preflight.completed = false;
      platformHub.publish(structuredClone(s.platform));
      await sleep(400);
      s.platform.preflight.completed = true;
      platformHub.publish(structuredClone(s.platform));
    },

    async listWorkspaces() {
      requireMe();
      return Object.values(s.workspaces).map((w) => ({
        name: w.name,
        displayName: w.displayName,
        rooms: s.rooms.filter((r) => r.workspace === w.name).length,
        ready: s.rooms.filter((r) => r.workspace === w.name).every((r) => r.phase !== "Provisioning"),
      }));
    },
    async getWorkspaceDraft(name) {
      const w = s.workspaces[name];
      if (!w) throw new ApiError("not_found", `Workspace ${name} not found.`);
      return structuredClone(w);
    },
    async listRoleTemplates() {
      return [
        { name: "coordinator", title: "Coordinator", summary: "Splits work into task cards and keeps the team moving." },
        { name: "implementer", title: "Implementer", summary: "Takes a task card and delivers a pull request." },
        { name: "reviewer", title: "Reviewer", summary: "Reviews every pull request against its task card." },
        { name: "ci-infra", title: "CI and infrastructure", summary: "Keeps CI green and the build fast." },
        { name: "qa", title: "QA", summary: "Tests the product end to end and files bugs." },
      ];
    },
    async listModels() {
      const fromProviders = s.platform.spec.services.providers.flatMap((p) => p.models);
      return fromProviders.length ? fromProviders : ["claude-opus-5-5", "claude-sonnet-5", "gpt-5.5"];
    },
    async suggestAgentNames(count, exclude) {
      return { names: pickNames(count, [...s.rooms.map((r) => r.name), ...exclude]), roles: defaultRoles(count) };
    },
    async planWorkspace(draft) {
      const old = s.workspaces[draft.name];
      const changes: PlanChange[] = [];
      changes.push({ op: old ? (JSON.stringify(old) === JSON.stringify(draft) ? "unchanged" : "update") : "create", kind: "Workspace", name: draft.name, details: [] });
      const roles = new Set(draft.rooms.map((r) => r.role));
      if (!old) for (const r of roles) changes.push({ op: "create", kind: "RoleTemplate", name: r, details: [] });
      for (const r of draft.rooms) {
        const prev = old?.rooms.find((x) => x.name === r.name);
        if (!prev) {
          changes.push({ op: "create", kind: "Room", name: r.name, details: [`${r.role}, ${r.cli}, ${r.model}`] });
          continue;
        }
        const details = (Object.keys(r) as (keyof RoomDraft)[])
          .filter((k) => JSON.stringify(r[k]) !== JSON.stringify(prev[k]))
          .map((k) => `${k}: ${JSON.stringify(prev[k])} -> ${JSON.stringify(r[k])}`);
        changes.push({ op: details.length ? "update" : "unchanged", kind: "Room", name: r.name, details });
      }
      for (const prev of old?.rooms ?? []) {
        if (!draft.rooms.some((r) => r.name === prev.name)) changes.push({ op: "delete", kind: "Room", name: prev.name, details: [] });
      }
      if (!old) changes.push({ op: "create", kind: "RunnerPool", name: "default", details: [] });
      // Capacity from the mock cluster: 15 CPU, 58 GiB memory, 820 GiB disk.
      const cpu = draft.rooms.reduce((a, r) => a + r.cpu, 0);
      const mem = draft.rooms.reduce((a, r) => a + r.memoryGi, 0);
      const disk = draft.rooms.reduce((a, r) => a + r.homeGi + r.engineGi, 0);
      const short = [cpu > 15 && "CPU", mem > 58 && "memory", disk > 820 && "disk"].filter(Boolean);
      return {
        changes,
        problems: validateDraft(draft),
        effective: effectiveOf(draft),
        fits: short.length === 0,
        fitNote: `${cpu} of 15 CPU, ${mem} of 58 GiB memory, ${disk} of 820 GiB disk${short.length ? `; short on ${short.join(", ")}` : ""}.`,
      };
    },
    async applyWorkspace(draft) {
      requireRole("admin", "operator");
      const old = s.workspaces[draft.name];
      const clean = structuredClone(draft);
      for (const r of clean.repositories) {
        r.importCredential = "";
        r.mirrorToken = "";
      }
      s.workspaces[draft.name] = clean;
      for (const r of draft.rooms) {
        const prev = old?.rooms.find((x) => x.name === r.name);
        if (prev && JSON.stringify(prev) === JSON.stringify(r)) continue;
        s.rooms = s.rooms.filter((x) => !(x.workspace === draft.name && x.name === r.name));
        const sum = { ...summaryFromDraft(draft.name, r, draft.perRoomMonthlyUSD), ticks: 0 };
        s.rooms.push(sum);
        upsert(sum);
      }
      for (const prev of old?.rooms ?? []) {
        if (!draft.rooms.some((r) => r.name === prev.name)) {
          s.rooms = s.rooms.filter((x) => !(x.workspace === draft.name && x.name === prev.name));
          fleetHub.publish({ kind: "remove", workspace: draft.name, name: prev.name });
        }
      }
      if (s.me && !s.me.workspaces.includes(draft.name)) s.me.workspaces.push(draft.name);
      if (s.platform.spec.wizardStep === "workspace") s.platform.spec.wizardStep = "done";
      audit("workspace.apply", `Workspace/${draft.name}`, `${draft.rooms.length} rooms`, draft.name);
      save();
    },
    async discoverGitHub(org, token, prefix) {
      if (!token) throw new ApiError("invalid_argument", "A token is required.");
      return {
        repos: [
          { name: "api", description: "The public API", private: false, archived: false, hasWiki: true, openIssues: 14 },
          { name: "web", description: "The web app", private: true, archived: false, hasWiki: false, openIssues: 6 },
          { name: "old-site", description: "", private: true, archived: true, hasWiki: false, openIssues: 0 },
        ],
        bots: ["athena", "hermes", "apollo"].map((n, i) => ({ login: `${prefix}${n}`, id: 9000 + i, room: n })),
        people: org.length,
      };
    },
    async defaultChannels(draft) {
      return defaultChatChannels(draft);
    },
    async listRoomSecrets(ws) {
      requireMe();
      return { secretName: "room-secrets", keys: Object.keys(s.roomSecrets[ws] ?? {}).sort() };
    },
    async setRoomSecret(ws, key, value) {
      requireRole("admin", "operator");
      if (!secretKeyPattern.test(key)) throw new ApiError("invalid_argument", "Keys are letters, digits, '.', '_' or '-'.");
      (s.roomSecrets[ws] ??= {})[key] = value;
      audit("roomSecret.set", `Secret/${ws}/room-secrets`, key, ws);
      save();
      return { name: "room-secrets", key };
    },
    async deleteRoomSecret(ws, key) {
      requireRole("admin", "operator");
      delete s.roomSecrets[ws]?.[key];
      audit("roomSecret.delete", `Secret/${ws}/room-secrets`, key, ws);
      save();
    },
    async getRoomSettings(ws, name) {
      requireMe();
      const d = draftOf(ws);
      const r = roomDraft(ws, name);
      return structuredClone({
        workspace: d.roomDefaults ?? emptySettings(),
        role: d.roleDefaults?.[r.role] ?? emptySettings(),
        room: r.settings ?? emptySettings(),
        effective: effectiveFor(r.name, r.role, d.roomDefaults, d.roleDefaults?.[r.role], r.settings),
        sharedVolumes: mountsOf(d, r),
      });
    },
    async updateRoomSettings(ws, name, settings: RoomSettings) {
      requireRole("admin", "operator");
      const probs = validateSettings(`room ${name}`, settings);
      if (probs.length) throw new ApiError("invalid_argument", probs.join("; "));
      const d = draftOf(ws);
      const r = roomDraft(ws, name);
      r.settings = structuredClone(settings);
      audit("room.settings", `Room/${ws}/${name}`, "", ws);
      save();
      return effectiveFor(r.name, r.role, d.roomDefaults, d.roleDefaults?.[r.role], r.settings);
    },
    async *watchProvisioning(ws, signal) {
      const snap = (): RoomProgress[] =>
        s.rooms
          .filter((r) => r.workspace === ws)
          .map((r) => ({
            name: r.name,
            phase: r.phase,
            conditions: ["ForgeAccountReady", "KeysReady", "GatewayKeyReady", "MessagingIdentityReady", "PodReady"].map(
              (t, i): Condition => ({
                type: t,
                status: r.phase !== "Provisioning" || i < r.ticks + 1 ? "True" : "False",
                reason: "",
                message: "",
              }),
            ),
          }));
      for await (const _ of provisionHub.subscribe(signal, [ws])) {
        if (_ === ws) yield snap();
      }
    },

    async *watchFleet(signal) {
      requireMe();
      const visible = () => s.rooms.filter((r) => s.me?.role === "admin" || s.me?.workspaces.includes(r.workspace)).map(strip);
      yield* fleetHub.subscribe(signal, [{ kind: "snapshot", rooms: visible() }]);
    },
    async getRoom(ws, name) {
      const r = room(ws, name);
      return {
        summary: strip(r),
        conditions: ["ForgeAccountReady", "KeysReady", "GatewayKeyReady", "MessagingIdentityReady", "PodReady"].map((t) => ({
          type: t,
          status: r.phase === "Provisioning" ? "Unknown" : "True",
          reason: "",
          message: "",
        })),
        imageVersion: "0.1.0",
        forgeUser: name,
        repos: s.workspaces[ws]?.repositories.map((x) => `${s.workspaces[ws].org}/${x.name}`) ?? [],
        presenceHistory: [
          { presence: "idle", at: new Date(Date.now() - 3_600_000).toISOString() },
          { presence: r.presence, at: now() },
        ],
        recentMessages: s.messages.filter((m) => m.from === `${name}@${ws}` || m.to === `${name}@${ws}`),
        inputTokens: 182_340,
        outputTokens: 24_112,
        contextTokens: 61_000,
      };
    },
    async *watchTranscript(ws, name, signal) {
      room(ws, name);
      const entries: TranscriptEntry[] = [
        { id: "1", at: now(), kind: "system", text: `Session started in ~/work (${name})` },
        { id: "2", at: now(), kind: "user", text: "Message from athena@acme: please paginate /v1/orders." },
        { id: "3", at: now(), kind: "assistant", text: "I'll look at the orders handler first." },
        { id: "4", at: now(), kind: "tool", text: "Bash: rg -n 'func listOrders' src" },
        { id: "5", at: now(), kind: "toolResult", text: "src/orders.go:42:func listOrders(w http.ResponseWriter, r *http.Request) {" },
      ];
      for (const e of entries) {
        if (signal.aborted) return;
        yield e;
      }
      let i = 6;
      while (!signal.aborted) {
        await sleep(2000);
        if (signal.aborted) return;
        yield { id: String(i++), at: now(), kind: "assistant", text: "Still working on it…" };
      }
    },
    async *watchLogs(ws, name, component, signal) {
      room(ws, name);
      const comps = component ? [component] : ["supervisor", "cli", "engine", "janitor"];
      let i = 0;
      while (!signal.aborted) {
        yield { text: `${now()} [${comps[i % comps.length]}] line ${i}` };
        i++;
        await sleep(i < 20 ? 10 : 1500);
      }
    },
    async getResources(ws, name) {
      room(ws, name);
      return {
        metricsAvailable: true,
        cpuMillis: 850,
        cpuLimitMillis: 2000,
        memoryBytes: 2.4 * GiB,
        memoryLimitBytes: 6 * GiB,
        netRxBytesPerSec: 12_000,
        netTxBytesPerSec: 3_400,
        processes: [
          { pid: 1, user: "root", cpuPercent: 0.1, memoryBytes: 30e6, command: "supervisor" },
          { pid: 52, user: name, cpuPercent: 12.5, memoryBytes: 410e6, command: "claude" },
          { pid: 77, user: name, cpuPercent: 30.2, memoryBytes: 900e6, command: "node test/run.js" },
        ],
      };
    },
    async getStorage(ws, name) {
      const r = room(ws, name);
      return {
        quotaEnforced: false,
        volumes: [
          { name: "home", usedBytes: r.diskUsedBytes * 0.3, capacityBytes: 30 * GiB },
          { name: "engine", usedBytes: r.diskUsedBytes * 0.7, capacityBytes: 60 * GiB },
        ],
        images: [
          { id: "sha256:1a2b", name: "docker.io/library/postgres:17", sizeBytes: 430e6, detail: "used by 1 container" },
          { id: "sha256:3c4d", name: "<none>", sizeBytes: 1.2e9, detail: "dangling" },
        ],
        containers: [{ id: "c0ffee", name: "test-db", sizeBytes: 20e6, detail: "Exited (0) 2 days ago" }],
        engineVolumes: [{ id: "v1", name: "pgdata", sizeBytes: 300e6, detail: "unreferenced" }],
        sharedDirs: Object.entries(s.workspaces[ws] ? mountsOf(s.workspaces[ws], roomDraft(ws, name)) : {}).map(([n, m]) => ({ name: n, readOnly: m === "ro" })),
      };
    },
    async prune(ws, name, kind) {
      requireRole("admin", "operator");
      room(ws, name);
      audit("room.prune", `Room/${ws}/${name}`, kind, ws);
      save();
      return { reclaimedBytes: 1.5e9 };
    },
    async roomAction(ws, name, action) {
      requireRole("admin", "operator");
      const r = room(ws, name);
      switch (action.kind) {
        case "pause":
          r.paused = true;
          r.phase = "Paused";
          r.presence = "offline";
          break;
        case "resume":
          r.paused = false;
          r.phase = "Running";
          r.presence = "idle";
          break;
        case "recreate":
          if (action.deleteHome && action.confirmName !== name) throw new ApiError("invalid_argument", "Type the room's name to delete its home.");
          r.phase = "Provisioning";
          r.ticks = 0;
          break;
        case "remove":
          s.rooms = s.rooms.filter((x) => x !== r);
          fleetHub.publish({ kind: "remove", workspace: ws, name });
          break;
        case "updateNow":
          r.updateAvailable = false;
          r.phase = "Updating";
          setTimeout(() => {
            r.phase = "Running";
            upsert(r);
            save();
          }, 500);
          break;
        case "quarantine":
          r.quarantined = true;
          r.quarantineReason = action.reason || `Quarantined by ${s.me?.handle}`;
          break;
        case "release":
          if (!action.note?.trim()) throw new ApiError("invalid_argument", "Say why it is safe to release the room.");
          r.quarantined = false;
          r.quarantineReason = "";
          break;
        case "rotate":
          break;
      }
      if (action.kind !== "remove") upsert(r);
      audit(`room.${action.kind}`, `Room/${ws}/${name}`, JSON.stringify(action), ws);
      save();
    },

    async listApprovals() {
      requireMe();
      return structuredClone(s.approvals.filter((a) => !a.answer && !a.withdrawn));
    },
    async *watchApprovals(signal) {
      requireMe();
      yield* approvalHub.subscribe(signal);
    },
    async getApproval(aid) {
      requireMe();
      const a = s.approvals.find((x) => x.id === aid);
      if (!a) throw new ApiError("not_found", "This approval no longer exists.");
      return structuredClone(a);
    },
    async answerApproval(aid, decision, message) {
      requireRole("admin", "operator");
      const a = s.approvals.find((x) => x.id === aid);
      if (!a) throw new ApiError("not_found", "This approval no longer exists.");
      if (a.answer) throw new ApiError("invalid_argument", `Already answered by ${a.answer.answeredBy}.`);
      a.answer = { decision, message, answeredBy: s.me!.handle, at: now() };
      approvalHub.publish({ kind: "answered", id: aid, answer: a.answer });
      const r = s.rooms.find((x) => x.workspace === a.workspace && x.name === a.room);
      if (r) {
        r.pendingApprovals = Math.max(0, r.pendingApprovals - 1);
        if (r.pendingApprovals === 0 && r.presence === "waitingOnHuman") {
          r.presence = "working";
          r.currentTask = a.summary;
        }
        upsert(r);
      }
      audit("approval.answer", `Room/${a.workspace}/${a.room}`, `${decision}: ${a.summary}`, a.workspace);
      save();
    },

    async listConversations() {
      const me = requireMe();
      return s.rooms
        .filter((r) => me.role === "admin" || me.workspaces.includes(r.workspace))
        .map((r) => {
          const addr = `${r.name}@${r.workspace}`;
          const msgs = s.messages.filter((m) => m.from === addr || m.to === addr);
          return {
            address: addr,
            displayName: r.displayName,
            presence: r.presence,
            lastMessage: msgs[msgs.length - 1],
            unread: msgs.filter((m) => m.from === addr).length ? 1 : 0,
          };
        })
        .sort((a, b) => (b.lastMessage?.at ?? "").localeCompare(a.lastMessage?.at ?? ""));
    },
    async listMessages(address) {
      requireMe();
      return s.messages.filter((m) => m.from === address || m.to === address);
    },
    async *watchMessages(signal) {
      requireMe();
      yield* messageHub.subscribe(signal);
    },
    async sendMessage(to, body) {
      const me = requireMe();
      const ws = to.split("@")[1] ?? "";
      const m: ChatMessage = { id: id("M"), from: `${me.handle}@${ws}`, to, body, at: now(), fromKind: "human" };
      s.messages.push(m);
      messageHub.publish(m);
      save();
      setTimeout(() => {
        const reply: ChatMessage = { id: id("M"), from: to, to: m.from, body: `Got it: "${body.slice(0, 40)}"`, at: now(), fromKind: "agent" };
        s.messages.push(reply);
        messageHub.publish(reply);
        save();
      }, 600);
      return m;
    },

    async queryAudit(f) {
      requireMe();
      return s.audit
        .filter((e) => !f.actor || e.actor.includes(f.actor))
        .filter((e) => !f.action || e.action.includes(f.action))
        .filter((e) => !f.workspace || e.workspace === f.workspace)
        .filter((e) => !f.since || e.at >= f.since)
        .filter((e) => !f.until || e.at <= f.until)
        .slice(0, f.limit ?? 500);
    },
    async getSpend() {
      requireMe();
      const rooms = s.rooms.map((r) => ({ workspace: r.workspace, room: r.name, usd: r.spendUSD, budgetUSD: r.budgetUSD, exhausted: r.budgetUSD > 0 && r.spendUSD >= r.budgetUSD }));
      const workspaces = [...new Set(s.rooms.map((r) => r.workspace))].map((ws) => {
        const mine = s.rooms.filter((r) => r.workspace === ws);
        const models = [...new Set(mine.map((r) => r.model))].map((model) => ({ model, usd: mine.filter((r) => r.model === model).reduce((a, r) => a + r.spendUSD, 0) }));
        return { workspace: ws, usd: mine.reduce((a, r) => a + r.spendUSD, 0), models };
      });
      return { available: true, updatedAt: now(), rooms, workspaces };
    },
    async listUsers() {
      requireRole("admin");
      return structuredClone(s.users);
    },
    async updateUser(uid, change) {
      requireRole("admin");
      const u = s.users.find((x) => x.id === uid);
      if (!u) throw new ApiError("not_found", "No such user.");
      if (change.role) {
        u.role = change.role;
        u.roleOverride = true;
      }
      if (change.workspaces) u.workspaces = change.workspaces;
      if (change.disabled !== undefined) u.disabled = change.disabled;
      audit("user.update", `User/${u.handle}`, JSON.stringify(change));
      save();
    },
    async getVapidPublicKey() {
      return "BEl62iUYgUivxIkv69yViEuiBIa-Ib9-SkvMeAtA3LFgDzkrxZJjSgSnfckjBJuBkr3qBUYIHBQFLXYp5Nksh8U";
    },
    async subscribePush(sub) {
      requireMe();
      if (sub.endpoint) s.pushSubs.push(sub.endpoint);
      save();
    },
    async unsubscribePush(endpoint) {
      s.pushSubs = s.pushSubs.filter((e) => e !== endpoint);
      save();
    },
    async getNotificationPrefs() {
      requireMe();
      return structuredClone(s.prefs);
    },
    async setNotificationPrefs(p) {
      requireMe();
      s.prefs = structuredClone(p);
      save();
    },
    async listChannels() {
      requireMe();
      const out: ChannelInfo[] = [];
      for (const [ws, d] of Object.entries(s.workspaces)) {
        for (const c of d.chatChannels) {
          const a = access(ws, c);
          if (!a) continue;
          out.push({
            workspace: ws,
            name: c.name,
            description: c.description,
            access: a,
            delivery: s.delivery[`${ws}/${c.name}`] ?? c.defaultDelivery,
            allowedDelivery: c.allowedDelivery.length ? c.allowedDelivery : ["push", "digest", "pull"],
            unread: unreadIn(ws, c.name),
            members: c.members.length,
          });
        }
      }
      return out;
    },
    async readMessages(ws, scope, opts) {
      requireMe();
      let items: InboxItem[] =
        scope === "direct" ? directItems(ws) : scope.startsWith("#") ? s.posts.filter((p) => p.workspace === ws && p.channel === scope.slice(1)) : [...s.posts.filter((p) => p.workspace === ws), ...directItems(ws)];
      items = items.map((p) => ({ ...p, read: p.read || s.readIds.includes(p.id) }));
      if (scope === "mentions") items = items.filter((p) => p.mentionsMe);
      if (opts.unreadOnly) items = items.filter((p) => !p.read);
      items.sort((a, b) => a.at.localeCompare(b.at) || a.id.localeCompare(b.id));
      let end = items.length;
      if (opts.cursor) end = Math.max(0, items.findIndex((p) => p.id === opts.cursor));
      const start = Math.max(0, end - opts.limit);
      const page = items.slice(start, end);
      return { items: page, nextCursor: start > 0 ? page[0]?.id ?? "" : "", unreadRemaining: items.filter((p) => !p.read).length };
    },
    async getMessage(ws, mid) {
      requireMe();
      const it = [...s.posts.filter((p) => p.workspace === ws), ...directItems(ws)].find((p) => p.id === mid);
      if (!it) throw new ApiError("not_found", "No such message.");
      return { ...it, read: it.read || s.readIds.includes(it.id) };
    },
    async unreadSummary() {
      requireMe();
      const out = { direct: 0, mentions: 0, channels: 0 };
      for (const [ws, d] of Object.entries(s.workspaces)) {
        for (const c of d.chatChannels) if (access(ws, c)) out.channels += unreadIn(ws, c.name);
        out.direct += directItems(ws).filter((i) => !i.read).length;
        out.mentions += s.posts.filter((p) => p.workspace === ws && p.mentionsMe && !s.readIds.includes(p.id)).length;
      }
      return out;
    },
    async post(ws, channel, body) {
      const me = requireMe();
      const p = newPost(ws, channel, me.handle, "human", body);
      p.read = true;
      s.posts.push(p);
      itemHub.publish(p);
      const agents = mentions(body).filter((m) => s.rooms.some((r) => r.workspace === ws && r.name === m));
      // A mentioned agent answers in the channel a moment later.
      if (agents[0]) {
        setTimeout(() => {
          const reply = newPost(ws, channel, agents[0], "agent", `@${me.handle} on it — I'll report back here.`);
          s.posts.push(reply);
          itemHub.publish(reply);
          save();
        }, 700);
      }
      save();
      return { id: p.id, mentioned: agents };
    },
    async setDelivery(ws, channel, mode) {
      requireMe();
      s.delivery[`${ws}/${channel}`] = mode;
      save();
    },
    async markRead(ws, scope, upTo) {
      requireMe();
      const items = scope.startsWith("#") ? s.posts.filter((p) => p.workspace === ws && p.channel === scope.slice(1)) : scope === "direct" ? directItems(ws) : [...s.posts.filter((p) => p.workspace === ws), ...directItems(ws)];
      items.sort((a, b) => a.at.localeCompare(b.at));
      const last = upTo ? items.findIndex((p) => p.id === upTo) : items.length - 1;
      for (const p of items.slice(0, last + 1)) if (!s.readIds.includes(p.id)) s.readIds.push(p.id);
      save();
    },
    async *watchItems(signal) {
      requireMe();
      yield* itemHub.subscribe(signal);
    },
    async mentionCandidates(ws) {
      const d = s.workspaces[ws];
      const rooms = s.rooms.filter((r) => r.workspace === ws);
      return [
        ...rooms.map((r) => ({ handle: r.name, kind: "agent" as const, detail: `${r.role}${r.group ? ` · ${r.group}` : ""}` })),
        ...s.users.map((u) => ({ handle: u.handle, kind: "person" as const, detail: u.displayName })),
        ...[...new Set(rooms.map((r) => r.role))].map((r) => ({ handle: `role:${r}`, kind: "group" as const, detail: "everyone with this role" })),
        ...(d?.groups ?? []).map((g) => ({ handle: `group:${g}`, kind: "group" as const, detail: "everyone in this group" })),
      ];
    },
    async listVerdicts(ws, room) {
      requireMe();
      return structuredClone(s.verdicts.filter((v) => v.workspace === ws && (!room || v.room === room))).sort((a, b) => b.at.localeCompare(a.at));
    },
    async decideVerdict(vid, decision) {
      requireRole("admin", "operator");
      const v = s.verdicts.find((x) => x.id === vid);
      if (!v) throw new ApiError("not_found", "No such verdict.");
      v.decision = { kind: decision, by: s.me!.handle, at: now() };
      audit(`verdict.${decision === "confirmed" ? "confirm" : "dismiss"}`, `Room/${v.workspace}/${v.room}`, `${v.severity}: ${v.reason}`, v.workspace);
      refreshVerdict(v.workspace, v.room);
      verdictHub.publish(structuredClone(v));
      save();
    },
    async *watchVerdicts(wss, signal) {
      requireMe();
      for await (const v of verdictHub.subscribe(signal)) if (!wss.length || wss.includes(v.workspace)) yield v;
    },
    async sendTestNotification(channel) {
      requireMe();
      audit("notify.test", `Channel/${channel}`);
    },
  };
  return api;
}
