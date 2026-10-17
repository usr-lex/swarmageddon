// The real API: the control center's Connect services (proto/swarm/controlcenter/v1),
// served under /api, mapped onto the UI's domain types. This is the only file
// that knows the generated messages.

import { clone, create, type Message } from "@bufbuild/protobuf";
import type { GenMessage } from "@bufbuild/protobuf/codegenv2";
import { timestampDate, timestampFromDate, type Timestamp } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, createClient, type Transport } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import * as pb from "@/gen/swarm/controlcenter/v1/controlcenter_pb";
import { ApiError, type Api } from "./api";
import { defaultStorm, defaultWarden } from "@/lib/workspace";
import type { AdvisorDraft, ChannelAccess, ChatChannelDraft, DeliveryMode, InboxItem, MentionCandidate, ReadScope, Severity, StormDraft, Verdict, WardenDraft, WardenMode } from "./channels";
import { emptySettings, type EffectiveSettings, type McpServer, type McpValue, type RoomSettings, type SettingSource, type SharedVolumeDraft } from "./settings";
import type { ActivityEvent, ActivityKind, AgentAxes, Edges, HandoffKind, MessageEdge, ObservatoryApi, ObsWindow, ThroughputDay } from "./observatory";
import type {
  Approval,
  ApprovalEvent,
  ChannelDraft,
  ChannelKind,
  ChatMessage,
  CheckResult,
  Condition,
  Decision,
  FleetEvent,
  Me,
  NotificationEvent,
  Platform,
  PlatformSpec,
  Presence,
  Role,
  RoomPhase,
  RoomProgress,
  RoomSummary,
  TranscriptEntry,
  User,
  WizardStep,
  WorkspaceDraft,
} from "./types";

const iso = (t?: Timestamp) => (t ? timestampDate(t).toISOString() : "");
const ts = (d: Date) => timestampFromDate(d);

// ---------------------------------------------------------------- enums

const roleFrom = (r: pb.Role): Role => (r === pb.Role.ADMIN ? "admin" : r === pb.Role.OPERATOR ? "operator" : "viewer");
const roleTo = (r: Role | ""): pb.Role => (r === "admin" ? pb.Role.ADMIN : r === "operator" ? pb.Role.OPERATOR : r === "viewer" ? pb.Role.VIEWER : pb.Role.UNSPECIFIED);

const presenceFrom = (p: pb.Presence): Presence =>
  p === pb.Presence.WORKING ? "working" : p === pb.Presence.WAITING_ON_HUMAN ? "waitingOnHuman" : p === pb.Presence.IDLE ? "idle" : "offline";

const decisionFrom = (d: pb.Decision): Decision => (d === pb.Decision.ALLOW_FOR_SESSION ? "allowForSession" : d === pb.Decision.DENY ? "deny" : "allow");
const decisionTo = (d: Decision): pb.Decision => (d === "allowForSession" ? pb.Decision.ALLOW_FOR_SESSION : d === "deny" ? pb.Decision.DENY : pb.Decision.ALLOW);

const conditionFrom = (c: pb.Condition): Condition => ({
  type: c.type,
  status: c.status === "True" || c.status === "False" ? c.status : "Unknown",
  reason: c.reason,
  message: c.message,
});

const severityFrom = (s: pb.Severity): Severity | undefined =>
  s === pb.Severity.CRITICAL ? "critical" : s === pb.Severity.HIGH ? "high" : s === pb.Severity.MEDIUM ? "medium" : s === pb.Severity.LOW ? "low" : undefined;

// ---------------------------------------------------------------- quantities

/** "6Gi" -> 6, "512Mi" -> 0.5, "1Ti" -> 1024. */
export function parseGi(q: string): number {
  const m = /^([0-9.]+)\s*(Ki|Mi|Gi|Ti|K|M|G|T)?$/.exec(q.trim());
  if (!m) return 0;
  const n = Number(m[1]);
  const f: Record<string, number> = { Ki: 1 / 1024 ** 2, Mi: 1 / 1024, Gi: 1, Ti: 1024, K: 1e3 / 1024 ** 3, M: 1e6 / 1024 ** 3, G: 1e9 / 1024 ** 3, T: 1e12 / 1024 ** 3 };
  return m[2] ? n * f[m[2]] : n / 1024 ** 3;
}

/** "2" -> 2, "500m" -> 0.5. */
export function parseCPU(q: string): number {
  return q.endsWith("m") ? Number(q.slice(0, -1)) / 1000 : Number(q) || 0;
}

const gi = (n: number) => `${n}Gi`;

// ---------------------------------------------------------------- rooms

/** Repeated unexpected CLI exits or a failed phase read as "crashed" (the proto has no crashed presence). */
export function roomFrom(r: pb.RoomSummary): RoomSummary {
  const used = r.volumes.reduce((a, v) => a + Number(v.usedBytes), 0);
  const cap = r.volumes.reduce((a, v) => a + Number(v.capacityBytes), 0);
  const crashed = r.crashCount >= 2 || r.phase === "Failed";
  return {
    workspace: r.workspace,
    name: r.name,
    displayName: r.displayName || r.name,
    role: r.role,
    group: r.group,
    presence: crashed ? "crashed" : presenceFrom(r.presence),
    cli: r.cli === "ore" ? "ore" : "claude",
    model: r.model,
    currentTask: r.currentTask,
    spendUSD: r.spendUsd,
    budgetUSD: 0,
    diskUsedBytes: used,
    diskCapacityBytes: cap,
    phase: (r.phase || "Provisioning") as RoomPhase,
    paused: r.paused,
    updateAvailable: r.updateAvailable,
    pendingApprovals: r.pendingApprovals,
    verdict: r.undecidedVerdicts > 0 ? severityFrom(r.wardenSeverity) : undefined,
    quarantined: r.quarantined,
    quarantineReason: r.quarantineReason,
  };
}

// ---------------------------------------------------------------- layered settings

const mcpValueFrom = (v: pb.McpValuePlan): McpValue => (v.valueFrom ? { value: "", valueFrom: { name: v.valueFrom.name, key: v.valueFrom.key } } : { value: v.value });
const mcpValueTo = (v: McpValue) => (v.valueFrom ? { valueFrom: { name: v.valueFrom.name, key: v.valueFrom.key } } : { value: v.value });
const mapValues = <A, B>(m: Record<string, A>, f: (a: A) => B): Record<string, B> => Object.fromEntries(Object.entries(m).map(([k, v]) => [k, f(v)]));

export function settingsFrom(p: pb.RoomSettingsPlan | undefined): RoomSettings {
  if (!p) return emptySettings();
  return {
    env: { ...p.env },
    secretEnv: p.secretEnv.map((e) => ({ name: e.name, secret: { name: e.secret?.name ?? "", key: e.secret?.key ?? "" } })),
    mcpServers: mapValues(
      p.mcpServers,
      (m): McpServer => ({
        type: m.type === "http" ? "http" : m.type === "stdio" ? "stdio" : "",
        command: m.command,
        args: m.args,
        env: mapValues(m.env, mcpValueFrom),
        url: m.url,
        headers: mapValues(m.headers, mcpValueFrom),
        enabledTools: m.enabledTools,
        disabled: m.disabled,
      }),
    ),
    network: p.network
      ? { mode: (["none", "limited", "full"].includes(p.network.mode) ? p.network.mode : "") as "", domains: p.network.domains, packageManagers: p.network.packageManagers }
      : undefined,
  };
}

export function settingsTo(s: RoomSettings | undefined): pb.RoomSettingsPlan {
  const x = s ?? emptySettings();
  return create(pb.RoomSettingsPlanSchema, {
    env: x.env,
    secretEnv: x.secretEnv.map((e) => ({ name: e.name, secret: e.secret })),
    mcpServers: mapValues(x.mcpServers, (m) => ({
      type: m.type,
      command: m.command,
      args: m.args,
      env: mapValues(m.env, mcpValueTo),
      url: m.url,
      headers: mapValues(m.headers, mcpValueTo),
      enabledTools: m.enabledTools,
      disabled: m.disabled,
    })),
    network: x.network ? { mode: x.network.mode, domains: x.network.domains, packageManagers: x.network.packageManagers } : undefined,
  });
}

const effectiveFrom = (e: pb.EffectiveSettings): EffectiveSettings => ({
  room: e.room,
  role: e.role,
  settings: settingsFrom(e.settings),
  sources: e.sources.map((x) => ({ kind: x.kind, key: x.key, level: x.level as SettingSource["level"] })),
});

const sharedVolumeFrom = (v: pb.SharedVolumePlan): SharedVolumeDraft => ({
  name: v.name,
  sizeGi: parseGi(v.size),
  storageClass: v.storageClass,
  mounts: v.mounts.map((m) => ({ select: m.select, readOnly: m.readOnly })),
});

// ---------------------------------------------------------------- channels and storm

/** "15m" -> 15, "1h30m" -> 90, "90s" -> 1.5. */
export function durationMinutes(d: string): number {
  let total = 0;
  for (const m of d.matchAll(/([0-9.]+)(h|m|s)/g)) total += Number(m[1]) * (m[2] === "h" ? 60 : m[2] === "s" ? 1 / 60 : 1);
  return total;
}

const delivery = (m: string): DeliveryMode => (m === "push" || m === "pull" ? m : "digest");
const access = (a: string): ChannelAccess => (a === "read" || a === "write" ? a : "readWrite");

const chatChannelFrom = (c: pb.ChannelPlan): ChatChannelDraft => ({
  name: c.name,
  description: c.description,
  members: c.members.map((m) => ({ select: m.select, access: access(m.access) })),
  defaultDelivery: delivery(c.defaultDelivery),
  allowedDelivery: c.allowedDelivery.map(delivery),
  postsPerMinute: c.postsPerMinute,
});

const chatChannelTo = (c: ChatChannelDraft) =>
  create(pb.ChannelPlanSchema, {
    name: c.name,
    description: c.description,
    members: c.members,
    defaultDelivery: c.defaultDelivery,
    allowedDelivery: c.allowedDelivery,
    postsPerMinute: c.postsPerMinute,
  });

const stormFrom = (s: pb.StormPlan | undefined): StormDraft =>
  s ? { pushPerMinute: s.pushPerMinute, coalesceBytes: s.coalesceBytes, digestMinutes: durationMinutes(s.digestInterval), fanoutPerMinute: s.fanoutPerMinute } : defaultStorm();

const itemFrom = (workspace: string, i: pb.ChannelItem): InboxItem => ({
  id: i.id,
  workspace,
  channel: i.channel,
  from: i.from,
  fromKind: i.fromKind === "human" ? "human" : "agent",
  body: i.body,
  at: iso(i.sentAt),
  read: i.read,
  mentionsMe: i.mentionsMe,
});

const scopeTo = (s: ReadScope) => (s.startsWith("#") ? s.slice(1) : s);

const verdictFrom = (v: pb.Verdict): Verdict => ({
  id: v.id,
  workspace: v.workspace,
  room: v.room,
  layer: (["rules", "judge", "trajectory", "gateway"].includes(v.layer) ? v.layer : "rules") as Verdict["layer"],
  severity: severityFrom(v.severity) ?? "low",
  rule: v.rule,
  reason: v.reason,
  evidence: v.evidence,
  action: v.action,
  source: v.source === "gateway" ? "gateway" : "room",
  at: iso(v.at),
  decision: v.decision ? { kind: v.decision === "dismiss" ? "dismissed" : "confirmed", by: v.decidedBy, at: iso(v.decidedAt) } : undefined,
});

function approvalFrom(a: pb.Approval): Approval {
  return {
    id: a.id,
    workspace: a.workspace,
    room: a.room,
    cli: a.cli === "ore" ? "ore" : "claude",
    tool: a.tool,
    summary: a.summary,
    detail: a.detail,
    reason: a.reason,
    matchingRules: a.matchingRules,
    sessionScopeAvailable: a.sessionScopeAvailable,
    requestedAt: iso(a.requestedAt),
    answer: a.state === "answered" ? { decision: decisionFrom(a.decision), message: a.answerMessage, answeredBy: a.answeredBy, at: iso(a.answeredAt) } : undefined,
    withdrawn: a.state === "withdrawn" ? "withdrawn by the room" : a.state === "expired" ? "expired" : undefined,
  };
}

function messageFrom(m: pb.InboxMessage): ChatMessage {
  return { id: m.id, from: m.from, to: m.to, body: m.body, at: iso(m.sentAt), fromKind: m.direction === "out" ? "human" : "agent" };
}

const peerOf = (m: pb.InboxMessage) => (m.direction === "out" ? m.to : m.from);

// ---------------------------------------------------------------- platform

function platformFrom(p: pb.Platform): Platform {
  const si = p.signIn;
  const sv = p.services;
  const hn = p.hostname;
  const st = p.status;
  const spec: PlatformSpec = {
    hostname: hn?.hostname ?? "",
    tlsMode: hn?.tlsMode === "certManager" ? "certManager" : "selfSigned",
    issuerName: hn?.issuerName ?? "",
    issuerKind: hn?.issuerKind ?? "",
    isolationTier: (["userns", "sysbox", "privileged"].includes(p.isolationTier) ? p.isolationTier : "auto") as PlatformSpec["isolationTier"],
    privilegedAccepted: p.privilegedAccepted,
    localDisabled: si ? !si.localEnabled : false,
    oidc: (si?.oidc ?? []).map((o) => ({
      name: o.name,
      displayName: o.displayName,
      issuer: o.issuer,
      clientID: o.clientId,
      groupsClaim: o.groupsClaim,
      scopes: o.scopes,
      clientSecretSet: o.clientSecretSet,
      redirectURL: o.redirectUrl,
    })),
    saml: (si?.saml ?? []).map((x) => ({
      name: x.name,
      displayName: x.displayName,
      metadataURL: x.metadataUrl,
      metadataXML: x.metadataXml,
      groupsAttribute: x.groupsAttribute,
      spMetadataURL: x.spMetadataUrl,
      acsURL: x.acsUrl,
    })),
    roleMappings: (si?.roleMappings ?? []).map((m) => ({ group: m.group, role: roleFrom(m.role) })),
    defaultRole: si && si.defaultRole !== pb.Role.UNSPECIFIED ? roleFrom(si.defaultRole) : "",
    services: {
      postgresStorage: sv?.postgresStorage ?? "",
      forgejoStorage: sv?.forgejoStorage ?? "",
      forgejoOIDCLogin: sv?.forgejoOidcLogin ?? false,
      messagingReplicas: sv?.messagingReplicas ?? 0,
      messagingStorage: sv?.messagingStorage ?? "",
      registryCacheStorage: sv?.registryCacheStorage ?? "",
      registryUpstreams: sv?.registryUpstreams ?? [],
      ntfyEnabled: sv?.ntfyEnabled ?? false,
      ntfyStorage: "",
      providers: (sv?.providers ?? []).map((x) => ({
        name: x.name,
        kind: x.kind === "openai" || x.kind === "openaiCompatible" ? x.kind : "anthropic",
        baseURL: x.baseUrl,
        models: x.models,
        hasKey: x.apiKeySet,
      })),
    },
    wizardStep: (p.wizardStep || "preflight") as WizardStep,
    servicesApproved: sv?.approved ?? false,
  };
  return {
    spec,
    adminClaimed: st?.adminClaimed ?? false,
    url: st?.url ?? "",
    caFingerprint: st?.caFingerprint ?? "",
    preflight: {
      checks: (st?.preflight ?? []).map((c) => ({ name: c.name, result: (["pass", "warn", "fail"].includes(c.result) ? c.result : "pending") as CheckResult, message: c.message, fix: c.fix })),
      tier: (st?.tier ?? "") as Platform["preflight"]["tier"],
      roomEstimate: st?.roomEstimate ?? 0,
      clusterCIDRs: [],
      completed: st?.preflightDone ?? false,
      rerunPending: !!st?.preflightRerunRequested && st.preflightRerunRequested !== st.preflightRerun,
    },
    conditions: (st?.conditions ?? []).map(conditionFrom),
    versions: st?.versions ?? {},
  };
}

// ---------------------------------------------------------------- workspaces

/** Parses an email channel target: smtp://user:pass@host:587?from=a@x&to=b@x,c@x */
export function parseSMTP(target: string) {
  try {
    const u = new URL(target);
    return {
      smtpHost: u.hostname,
      smtpPort: Number(u.port) || 587,
      smtpUsername: decodeURIComponent(u.username),
      smtpPassword: decodeURIComponent(u.password),
      emailFrom: u.searchParams.get("from") ?? "",
      emailTo: u.searchParams.get("to") ?? "",
    };
  } catch {
    return null;
  }
}

const wardenMode = (m: string): WardenMode | undefined => (m === "off" || m === "monitor" || m === "gate" ? m : undefined);

const advisorFrom = (a: pb.AdvisorPlan): AdvisorDraft => ({
  model: a.model,
  effort: a.effort || "high",
  maxCallsPerHour: a.maxCallsPerHour || 10,
  monthlyUSD: a.monthlyUsd,
  native: a.native,
});

const advisorTo = (a: AdvisorDraft) =>
  create(pb.AdvisorPlanSchema, { model: a.model, effort: a.model ? a.effort : "", maxCallsPerHour: a.maxCallsPerHour, monthlyUsd: a.monthlyUSD, native: a.native });

/** The full warden plan when the server sends one, else the mode and model over the defaults. */
function wardenFrom(p: pb.WorkspacePlan): WardenDraft {
  const w = p.warden;
  if (w) return { mode: wardenMode(w.mode) ?? "monitor", model: w.model, interruptOnHigh: w.interruptOnHigh, quarantineOnHigh: w.quarantineOnHigh, monthlyUSD: w.monthlyUsd || 20 };
  return { ...defaultWarden(), mode: wardenMode(p.wardenMode) ?? defaultWarden().mode, model: p.wardenModel };
}

function draftFrom(p: pb.WorkspacePlan): WorkspaceDraft {
  return {
    name: p.name,
    displayName: p.displayName,
    org: p.org,
    defaultModel: p.defaultModel,
    perRoomMonthlyUSD: p.budgetPerRoomMonthlyUsd,
    rooms: p.agents.map((a) => ({
      name: a.name,
      role: a.role,
      cli: a.cli === "ore" ? "ore" : "claude",
      model: a.model,
      effort: a.effort,
      group: a.group,
      cpu: parseCPU(a.cpu),
      memoryGi: parseGi(a.memory),
      homeGi: parseGi(a.homeVolume),
      engineGi: parseGi(a.engineVolume),
      repos: a.repos,
      settings: settingsFrom(a.settings),
      advisor: a.advisor ? (a.advisor.model ? advisorFrom(a.advisor) : null) : undefined,
      wardenMode: wardenMode(a.wardenMode) ?? "",
    })),
    groups: p.groups.map((g) => g.name),
    defaultLinks: p.defaultLinks === "none" ? "none" : "withinGroup",
    links: p.links.map((l) => ({
      from: l.from,
      to: l.to,
      mode: l.mode === "open" || l.mode === "none" ? l.mode : "mediated",
      senders: l.senders,
      recipients: l.recipients,
      requirePrefix: l.requirePrefix,
    })),
    repositories: p.repositories.map((r) => ({ name: r.name, importURL: r.importUrl, importCredential: "", group: r.group, pushMirror: r.pushMirror, mirrorToken: "" })),
    // Write-only channel settings come back empty; an empty target keeps what's stored.
    channels: p.notificationChannels.map((c) => ({ kind: c.kind as ChannelKind, target: c.kind === "ntfy" ? c.topic : "" })),
    events: p.notificationEvents as NotificationEvent[],
    chatChannels: p.channels.map(chatChannelFrom),
    storm: stormFrom(p.storm),
    warden: wardenFrom(p),
    advisors: mapValues(p.roleAdvisors, advisorFrom),
    roomDefaults: settingsFrom(p.roomDefaults),
    roleDefaults: mapValues(p.roleDefaults, settingsFrom),
    sharedVolumes: p.sharedVolumes.map(sharedVolumeFrom),
    source: p,
  };
}

/** Starts from the server's message when there is one, so fields the wizard doesn't edit survive. */
function from<T extends Message>(schema: GenMessage<T>, src: T | undefined, patch: Partial<T>): T {
  return Object.assign(src ? clone(schema, src) : create(schema), patch);
}

function planTo(d: WorkspaceDraft): pb.WorkspacePlan {
  const src = d.source as pb.WorkspacePlan | undefined;
  const srcAgent = new Map(src?.agents.map((a) => [a.name, a]) ?? []);
  const srcGroup = new Map(src?.groups.map((g) => [g.name, g]) ?? []);
  const srcRepo = new Map(src?.repositories.map((r) => [r.name, r]) ?? []);
  const srcChannel = new Map(src?.notificationChannels.map((c) => [c.kind, c]) ?? []);
  const channel = (c: ChannelDraft) => {
    const out = from(pb.NotificationChannelPlanSchema, srcChannel.get(c.kind), { kind: c.kind });
    if (c.kind === "ntfy") out.topic = c.target;
    if ((c.kind === "slack" || c.kind === "discord") && c.target) out.webhookUrl = c.target;
    if (c.kind === "email" && c.target) Object.assign(out, parseSMTP(c.target) ?? {});
    return out;
  };
  return from(pb.WorkspacePlanSchema, src, {
    name: d.name,
    displayName: d.displayName,
    org: d.org,
    defaultModel: d.defaultModel,
    budgetPerRoomMonthlyUsd: d.perRoomMonthlyUSD,
    agents: d.rooms.map((r) =>
      from(pb.AgentPlanSchema, srcAgent.get(r.name), {
        name: r.name,
        role: r.role,
        cli: r.cli,
        model: r.model,
        effort: r.effort,
        group: r.group,
        cpu: String(r.cpu),
        memory: gi(r.memoryGi),
        homeVolume: gi(r.homeGi),
        engineVolume: gi(r.engineGi),
        repos: r.repos,
        settings: settingsTo(r.settings),
        // undefined inherits the role's advisor; null turns it off (an empty model).
        advisor: r.advisor === undefined ? undefined : advisorTo(r.advisor ?? { model: "", effort: "", maxCallsPerHour: 0, monthlyUSD: 0, native: false }),
        wardenMode: r.wardenMode ?? "",
      }),
    ),
    groups: d.groups.map((g) => from(pb.GroupPlanSchema, srcGroup.get(g), { name: g })),
    defaultLinks: d.defaultLinks,
    links: d.links.map((l) => create(pb.LinkPlanSchema, { from: l.from, to: l.to, mode: l.mode, senders: l.senders, recipients: l.recipients, requirePrefix: l.requirePrefix })),
    // Credentials are write-only: an empty one keeps the stored Secret.
    repositories: d.repositories.map((r) =>
      from(pb.RepositoryPlanSchema, srcRepo.get(r.name), {
        name: r.name,
        importUrl: r.importURL,
        group: r.group,
        pushMirror: r.pushMirror,
        importCredential: r.importCredential,
        mirrorToken: r.mirrorToken,
      }),
    ),
    notificationChannels: d.channels.map(channel),
    notificationEvents: d.events,
    channels: d.chatChannels.map(chatChannelTo),
    storm: create(pb.StormPlanSchema, { pushPerMinute: d.storm.pushPerMinute, coalesceBytes: d.storm.coalesceBytes, digestInterval: `${d.storm.digestMinutes}m`, fanoutPerMinute: d.storm.fanoutPerMinute }),
    wardenMode: d.warden.mode,
    wardenModel: d.warden.model,
    warden: create(pb.WardenPlanSchema, {
      mode: d.warden.mode,
      model: d.warden.model,
      interruptOnHigh: d.warden.interruptOnHigh,
      quarantineOnHigh: d.warden.quarantineOnHigh,
      monthlyUsd: d.warden.monthlyUSD,
    }),
    // Roles the draft names replace the template's advisor; an empty model removes it.
    roleAdvisors: mapValues(d.advisors, advisorTo),
    roomDefaults: settingsTo(d.roomDefaults),
    roleDefaults: mapValues(d.roleDefaults, settingsTo),
    sharedVolumes: d.sharedVolumes.map((v) => create(pb.SharedVolumePlanSchema, { name: v.name, size: gi(v.sizeGi), storageClass: v.storageClass, mounts: v.mounts })),
  });
}

/** One transcript item as the Agent tab shows it. */
export function transcriptFrom(i: pb.TranscriptItem): TranscriptEntry {
  const kind = i.role === "tool_result" ? "toolResult" : i.role === "tool" || i.role === "user" || i.role === "assistant" ? i.role : "system";
  const text = kind === "tool" ? `${i.toolName}: ${i.toolInput}` : kind === "toolResult" ? i.toolResult : i.text;
  return { id: i.id, at: iso(i.at), kind, text, toolName: i.toolName || undefined };
}

const changeOp = (a: pb.ChangeAction) =>
  a === pb.ChangeAction.CREATE ? "create" : a === pb.ChangeAction.UPDATE ? "update" : a === pb.ChangeAction.DELETE ? "delete" : "unchanged";

// ---------------------------------------------------------------- errors

const codes: Partial<Record<Code, ApiError["code"]>> = {
  [Code.Unauthenticated]: "unauthenticated",
  [Code.PermissionDenied]: "permission_denied",
  [Code.InvalidArgument]: "invalid_argument",
  [Code.FailedPrecondition]: "invalid_argument",
  [Code.NotFound]: "not_found",
  [Code.ResourceExhausted]: "resource_exhausted",
  [Code.Unimplemented]: "unimplemented",
};

function toApiError(e: unknown): unknown {
  if (e instanceof ConnectError) return new ApiError(codes[e.code] ?? "unknown", e.rawMessage || e.message);
  return e;
}


/** Wraps every method so Connect errors surface as ApiError, for promises and streams alike. */
function withErrors<T extends object>(o: T): T {
  for (const [k, v] of Object.entries(o)) {
    if (typeof v !== "function") continue;
    (o as Record<string, unknown>)[k] = (...args: unknown[]) => {
      const r = v.apply(o, args);
      if (r && typeof (r as Promise<unknown>).then === "function") return (r as Promise<unknown>).catch((e) => Promise.reject(toApiError(e)));
      if (r && Symbol.asyncIterator in r) {
        return (async function* () {
          try {
            yield* r as AsyncIterable<unknown>;
          } catch (e) {
            throw toApiError(e);
          }
        })();
      }
      return r;
    };
  }
  return o;
}

// ---------------------------------------------------------------- the API

const windowMs: Record<ObsWindow, number> = { "1h": 3_600_000, "24h": 86_400_000, "7d": 7 * 86_400_000, "30d": 30 * 86_400_000 };

const handoffKind = (s: string): HandoffKind => (s === "assigned" ? "assigned" : s === "mentioned" ? "mentioned" : "reviewRequested");

export function createConnectApi(transport: Transport = createConnectTransport({ baseUrl: "/api" })): Api {

  const setup = createClient(pb.SetupService, transport);
  const auth = createClient(pb.AuthService, transport);
  const platform = createClient(pb.PlatformService, transport);
  const workspaces = createClient(pb.WorkspaceService, transport);
  const rooms = createClient(pb.RoomService, transport);
  const fleet = createClient(pb.FleetService, transport);
  const inbox = createClient(pb.InboxService, transport);
  const approvals = createClient(pb.ApprovalService, transport);
  const auditSvc = createClient(pb.AuditService, transport);
  const notify = createClient(pb.NotificationService, transport);
  const spend = createClient(pb.SpendService, transport);
  const users = createClient(pb.UserService, transport);
  const obs = createClient(pb.ObservatoryService, transport);
  const channels = createClient(pb.ChannelService, transport);
  const warden = createClient(pb.WardenService, transport);
  const myWorkspaces = async () => (me ?? (await api.me())).workspaces;

  const ref = (workspace: string, name: string) => ({ room: { workspace, name } });

  // Rooms of a workspace, cached briefly: the observatory joins them onto its aggregates.
  const roomCache = new Map<string, { at: number; rooms: Promise<pb.RoomSummary[]> }>();
  const listRooms = (workspace: string) => {
    const c = roomCache.get(workspace);
    if (c && Date.now() - c.at < 10_000) return c.rooms;
    const p = rooms.listRooms({ workspace }).then((r) => r.rooms);
    roomCache.set(workspace, { at: Date.now(), rooms: p });
    p.catch(() => roomCache.delete(workspace));
    return p;
  };
  /** Party names may be addresses (name@ws); agents of this workspace are shown by name, people as @handle. */
  const party = (p: string, workspace: string, agents: Set<string>) => {
    if (p.startsWith("#")) return p;
    const name = p.endsWith(`@${workspace}`) ? p.slice(0, -workspace.length - 1) : p;
    return agents.has(name) ? name : `@${name.split("@")[0]}`;
  };

  const eventFrom = (e: pb.ActivityEvent): ActivityEvent => ({
    id: String(e.id),
    at: iso(e.at),
    actor: e.actor,
    kind: e.kind as ActivityKind,
    target: e.target,
    repo: e.repo,
    summary: e.summary,
    url: e.url || undefined,
  });

  let me: Me | undefined;

  const observatory: ObservatoryApi = withErrors({
    async edges(workspace: string, w: ObsWindow): Promise<Edges> {
      const [res, rs] = await Promise.all([obs.edges({ workspace, since: ts(new Date(Date.now() - windowMs[w])) }), listRooms(workspace)]);
      const names = new Set(rs.map((r) => r.name));
      const messages: MessageEdge[] = [];
      const handoffs: Edges["handoffs"] = [];
      const activity = new Map<string, number>();
      for (const e of res.edges) {
        const from = party(e.from, workspace, names);
        const to = party(e.to, workspace, names);
        const count = Number(e.count);
        activity.set(from, (activity.get(from) ?? 0) + count);
        activity.set(to, (activity.get(to) ?? 0) + count);
        if (e.kind === pb.EdgeKind.FORGE_HANDOFF) handoffs.push({ from, to, kind: handoffKind(e.subkind), count });
        else
          messages.push({
            from,
            to,
            count,
            kind: e.kind === pb.EdgeKind.CHANNEL_POST ? "channel" : e.kind === pb.EdgeKind.CROSSING ? "mediated" : e.kind === pb.EdgeKind.HUMAN ? "human" : "open",
          });
      }
      return {
        agents: rs.map((r) => ({ name: r.name, group: r.group, role: r.role, presence: roomFrom(r).presence, activity: activity.get(r.name) ?? 0 })),
        humans: [...new Set(messages.flatMap((m) => [m.from, m.to]).filter((p) => p.startsWith("@")))],
        channels: [...new Set(messages.flatMap((m) => [m.from, m.to]).filter((p) => p.startsWith("#")))],
        messages,
        handoffs,
      };
    },
    async pairMessages(workspace: string, a: string, b: string, w: ObsWindow) {
      // Message metadata comes from the timeline: events by either party aimed at the other.
      const res = await obs.timeline({ workspace, actors: [a, b], kinds: ["message", "crossing", "human"], limit: 500 });
      const since = Date.now() - windowMs[w];
      return res.events
        .filter((e) => (e.target === a || e.target === b || e.target.startsWith(`${a}@`) || e.target.startsWith(`${b}@`)) && timestampDate(e.at!).getTime() >= since)
        .map((e) => ({
          id: String(e.id),
          at: iso(e.at),
          from: e.actor,
          to: e.target,
          kind: e.kind === "crossing" ? ("mediated" as const) : e.kind === "human" ? ("human" as const) : ("open" as const),
        }));
    },
    async now(workspace: string) {
      const [res, rs] = await Promise.all([obs.now({ workspace }), listRooms(workspace)]);
      const byName = new Map(rs.map((r) => [r.name, r]));
      return res.agents.map((a) => {
        const r = byName.get(a.name);
        const presence = r ? roomFrom(r).presence : presenceFrom(a.presence);
        return {
          name: a.name,
          role: a.role || r?.role || "",
          group: r?.group ?? "",
          presence,
          currentTask: a.currentTask,
          lastTouched: a.lastItem
            ? { kind: /\/pulls?\//.test(a.lastItem.url) ? ("pr" as const) : ("issue" as const), repo: a.lastItem.repo, number: a.lastItem.number, title: a.lastItem.title }
            : undefined,
          since: iso(a.presenceSince),
        };
      });
    },
    async heatmap(workspace: string, days: number) {
      const res = await obs.heatmap({ workspace, days });
      return res.cells.map((c) => ({ day: c.day, agent: c.actor, count: Number(c.count) }));
    },
    async axes(workspace: string, w: ObsWindow): Promise<AgentAxes[]> {
      const [res, rs] = await Promise.all([obs.axes({ workspace, since: ts(new Date(Date.now() - windowMs[w])) }), listRooms(workspace)]);
      const role = new Map(rs.map((r) => [r.name, r.role]));
      return res.rows.map((r) => ({
        name: r.actor,
        role: role.get(r.actor) ?? "",
        commits: Number(r.commits),
        pullRequests: Number(r.pullRequests),
        reviews: Number(r.reviews),
        issues: Number(r.issues),
        messages: Number(r.messages),
      }));
    },
    async timeline(workspace: string, f) {
      const res = await obs.timeline({ workspace, kinds: f.kinds ?? [], actors: f.actor ? [f.actor] : [], limit: f.limit ?? 200 });
      return res.events.map(eventFrom);
    },
    async *watchTimeline(workspace: string, signal: AbortSignal) {
      for await (const r of obs.watchTimeline({ workspace }, { signal })) if (r.event) yield eventFrom(r.event);
    },
    async throughput(workspace: string, days: number) {
      const res = await obs.throughput({ workspace, days });
      const byDay = new Map<string, ThroughputDay>();
      const day = (d: string) => {
        let x = byDay.get(d);
        if (!x) byDay.set(d, (x = { day: d, prsMerged: {}, reviewLatencyHours: null, ciFailures: 0, tokens: {} }));
        return x;
      };
      for (const d of res.days) {
        const x = day(d.day);
        x.prsMerged[d.actor] = Number(d.merged);
        x.tokens[d.actor] = Number(d.tokens);
        x.ciFailures += Number(d.ciFailures);
      }
      for (const l of res.reviewLatency) if (Number(l.reviews) > 0) day(l.day).reviewLatencyHours = Math.round((l.medianSeconds / 3600) * 10) / 10;
      return {
        days: [...byDay.values()].sort((a, b) => a.day.localeCompare(b.day)),
        spendUSD: Object.fromEntries(res.spend.map((s) => [s.room, s.spendUsd])),
      };
    },
    async latestReport(workspace: string) {
      const res = await obs.reports({ workspace, limit: 1 });
      const r = res.reports[0];
      return r ? { id: String(r.id), at: iso(r.at), author: r.sender, markdown: r.body } : null;
    },
    async reports(workspace: string, limit: number) {
      const res = await obs.reports({ workspace, limit });
      return res.reports.map((r) => ({ id: String(r.id), at: iso(r.at), author: r.sender, markdown: r.body }));
    },
    async requestReport(workspace: string) {
      await obs.requestReport({ workspace });
    },
  } satisfies ObservatoryApi);

  const api: Omit<Api, "observatory"> = {
    async getSetupState() {
      const r = await setup.getSetupStatus({});
      return { adminClaimed: r.adminClaimed, databaseReady: r.databaseReady, caFingerprint: r.caFingerprint, url: r.url };
    },
    async claimAdmin(token, admin) {
      await setup.claimAdmin({ token: token.trim(), username: admin?.username ?? "", password: admin?.password ?? "", email: admin?.email ?? "" });
    },

    async me() {
      const r = await auth.getMe({});
      const u = r.user;
      if (!u) throw new ApiError("unauthenticated", "Sign in first.");
      me = { handle: u.handle, email: u.email, displayName: u.displayName || u.handle, role: roleFrom(u.role), workspaces: u.workspaces };
      return me;
    },
    async listAuthProviders() {
      const r = await auth.listProviders({});
      return r.providers
        .filter((p) => p.kind === pb.ProviderKind.OIDC || p.kind === pb.ProviderKind.SAML)
        .map((p) => ({ kind: p.kind === pb.ProviderKind.SAML ? ("saml" as const) : ("oidc" as const), name: p.name, displayName: p.displayName || p.name, loginURL: p.loginUrl }));
    },
    async loginLocal(username, password) {
      await auth.localLogin({ username, password });
    },
    async logout() {
      await auth.logout({});
      me = undefined;
    },

    async getPlatform() {
      const r = await platform.getPlatform({});
      return platformFrom(r.platform!);
    },
    async *watchPlatform(signal) {
      for await (const r of platform.watchPlatform({}, { signal })) if (r.platform) yield platformFrom(r.platform);
    },
    async updateIsolation(tier, privilegedAccepted) {
      const r = await platform.setIsolation({ tier, privilegedAccepted });
      return platformFrom(r.platform!);
    },
    async updateSignIn(spec, secrets) {
      const r = await platform.updateSignIn({
        signIn: {
          localEnabled: !spec.localDisabled,
          oidc: spec.oidc.map((o) => ({
            name: o.name,
            displayName: o.displayName,
            issuer: o.issuer,
            clientId: o.clientID,
            clientSecret: secrets?.oidcClientSecrets?.[o.name] ?? "",
            groupsClaim: o.groupsClaim,
            scopes: o.scopes,
          })),
          saml: spec.saml.map((x) => ({ name: x.name, displayName: x.displayName, metadataUrl: x.metadataURL, metadataXml: x.metadataXML, groupsAttribute: x.groupsAttribute })),
          roleMappings: spec.roleMappings.map((m) => ({ group: m.group, role: roleTo(m.role) })),
          defaultRole: roleTo(spec.defaultRole),
        },
      });
      // Forgejo's OIDC login is a services setting.
      if (r.platform && (r.platform.services?.forgejoOidcLogin ?? false) !== spec.services.forgejoOIDCLogin) {
        return api.updateServices({ ...platformFrom(r.platform).spec, services: { ...platformFrom(r.platform).spec.services, forgejoOIDCLogin: spec.services.forgejoOIDCLogin } });
      }
      return platformFrom(r.platform!);
    },
    async updateServices(spec, secrets) {
      const s = spec.services;
      const r = await platform.updateServices({
        services: {
          postgresStorage: s.postgresStorage,
          forgejoStorage: s.forgejoStorage,
          forgejoOidcLogin: s.forgejoOIDCLogin,
          messagingReplicas: s.messagingReplicas,
          messagingStorage: s.messagingStorage,
          providers: s.providers.map((p) => ({ name: p.name, kind: p.kind, baseUrl: p.baseURL, models: p.models, apiKey: secrets?.providerKeys?.[p.name] ?? "" })),
          registryCacheStorage: s.registryCacheStorage,
          registryUpstreams: s.registryUpstreams,
          ntfyEnabled: s.ntfyEnabled,
          approved: spec.servicesApproved,
        },
      });
      return platformFrom(r.platform!);
    },
    async updateHostname(spec) {
      const r = await platform.updateHostname({ hostname: { hostname: spec.hostname, tlsMode: spec.tlsMode, issuerName: spec.issuerName, issuerKind: spec.issuerKind } });
      return platformFrom(r.platform!);
    },
    async setWizardStep(step) {
      await platform.setWizardStep({ step });
    },
    async rerunPreflight() {
      await platform.rerunPreflight({});
    },

    async listWorkspaces() {
      const r = await workspaces.listWorkspaces({});
      return r.workspaces.map((w) => ({
        name: w.name,
        displayName: w.displayName,
        rooms: w.rooms,
        ready: w.conditions.some((c) => c.type === "Ready" && c.status === "True"),
      }));
    },
    async getWorkspaceDraft(name) {
      const r = await workspaces.getWorkspace({ name });
      return draftFrom(r.plan!);
    },
    async listRoleTemplates() {
      const r = await workspaces.listRoleTemplates({});
      return r.templates.map((t) => ({ name: t.name, title: t.title, summary: t.summary }));
    },
    async listModels() {
      return (await workspaces.listModels({})).models;
    },
    async suggestAgentNames(count, exclude) {
      const r = await workspaces.suggestAgentNames({ count, exclude });
      return { names: r.names, roles: r.roles };
    },
    async planWorkspace(draft) {
      const r = await workspaces.previewWorkspace({ plan: planTo(draft), pruneRooms: true });
      return {
        changes: r.changes.map((c) => ({ op: changeOp(c.action), kind: c.kind as "Workspace", name: c.name, details: c.fields })),
        problems: r.problems,
        fits: r.fits,
        fitNote: r.fitNote,
        effective: r.effective.map(effectiveFrom),
      };
    },
    async applyWorkspace(draft) {
      await workspaces.applyWorkspace({ plan: planTo(draft), pruneRooms: true });
    },
    async defaultChannels(draft) {
      const r = await workspaces.defaultChannels({ plan: planTo(draft) });
      return r.channels.map(chatChannelFrom);
    },
    async listRoomSecrets(workspace) {
      const r = await workspaces.listRoomSecrets({ workspace });
      return { secretName: r.secretName, keys: r.keys };
    },
    async setRoomSecret(workspace, key, value) {
      const r = await workspaces.setRoomSecret({ workspace, key, value });
      return { name: r.ref?.name ?? "room-secrets", key: r.ref?.key ?? key };
    },
    async deleteRoomSecret(workspace, key) {
      await workspaces.deleteRoomSecret({ workspace, key });
    },
    async *watchProvisioning(workspace, signal) {
      const state = new Map<string, RoomProgress>();
      const put = (r: pb.RoomSummary) => r.workspace === workspace && state.set(r.name, { name: r.name, phase: (r.phase || "Provisioning") as RoomPhase, conditions: r.conditions.map(conditionFrom) });
      for await (const ev of fleet.watchFleet({ workspaces: [workspace] }, { signal })) {
        if (ev.event.case === "snapshot") {
          state.clear();
          ev.event.value.rooms.forEach(put);
        } else if (ev.event.case === "updated") put(ev.event.value);
        else if (ev.event.case === "removed") state.delete(ev.event.value.name);
        yield [...state.values()];
      }
    },

    async *watchFleet(signal): AsyncGenerator<FleetEvent> {
      for await (const ev of fleet.watchFleet({}, { signal })) {
        if (ev.event.case === "snapshot") yield { kind: "snapshot", rooms: ev.event.value.rooms.map(roomFrom) };
        else if (ev.event.case === "updated") yield { kind: "upsert", room: roomFrom(ev.event.value) };
        else if (ev.event.case === "removed") yield { kind: "remove", workspace: ev.event.value.workspace, name: ev.event.value.name };
      }
    },
    async getRoom(workspace, name) {
      const [r, msgs] = await Promise.all([rooms.getRoom(ref(workspace, name)), inbox.listInbox({ workspace, peer: `${name}@${workspace}`, limit: 10 }).catch(() => null)]);
      const s = r.room!;
      return {
        summary: roomFrom(s),
        conditions: s.conditions.map(conditionFrom),
        imageVersion: r.imageVersion,
        forgeUser: r.forgeUser,
        repos: [],
        presenceHistory: [{ presence: presenceFrom(s.presence), at: iso(s.presenceSince) }],
        recentMessages: msgs?.messages.map(messageFrom) ?? [],
        inputTokens: Number(r.inputTokens),
        outputTokens: Number(r.outputTokens),
        contextTokens: Number(r.contextTokens),
      };
    },
    async *watchTranscript(workspace, name, signal) {
      for await (const r of rooms.watchTranscript({ ...ref(workspace, name), tailLines: 200 }, { signal })) if (r.item) yield transcriptFrom(r.item);
    },
    async *watchLogs(workspace, name, component, signal) {
      for await (const r of rooms.streamRoomLogs({ ...ref(workspace, name), follow: true, tailLines: 500n, component }, { signal })) yield { text: r.line };
    },
    async getResources(workspace, name) {
      const r = await rooms.getRoomResources(ref(workspace, name));
      return {
        metricsAvailable: r.metricsAvailable,
        cpuMillis: Number(r.cpuMillicores),
        cpuLimitMillis: Number(r.cpuLimitMillicores),
        memoryBytes: Number(r.memoryBytes),
        memoryLimitBytes: Number(r.memoryLimitBytes),
      };
    },
    async getStorage(workspace, name) {
      const r = await rooms.getRoomStorage(ref(workspace, name));
      return {
        volumes: r.volumes.map((v) => ({ name: v.name, usedBytes: Number(v.usedBytes), capacityBytes: Number(v.capacityBytes) })),
        sharedDirs: r.sharedDirs.map((d) => ({ name: d, readOnly: false })),
      };
    },
    async prune(workspace, name, kind) {
      const r = await rooms.pruneEngine({ ...ref(workspace, name), kind });
      return { reclaimedBytes: Number(r.reclaimedBytes), output: r.output };
    },
    async roomAction(workspace, name, a) {
      const r = ref(workspace, name);
      switch (a.kind) {
        case "pause":
          await rooms.pauseRoom(r);
          break;
        case "resume":
          await rooms.resumeRoom(r);
          break;
        case "recreate":
          await rooms.recreateRoom({ ...r, deleteHome: a.deleteHome, confirmName: a.confirmName ?? "" });
          break;
        case "remove":
          await rooms.removeRoom({ ...r, deleteDataNow: a.deleteDataNow, confirmName: name });
          break;
        case "rotate":
          await rooms.rotateRoomCredentials(r);
          break;
        case "updateNow":
          await rooms.updateRoomNow(r);
          break;
        case "quarantine":
          await rooms.quarantineRoom({ ...r, reason: a.reason });
          break;
        case "release":
          await rooms.releaseRoom({ ...r, note: a.note ?? "" });
          break;
      }
    },
    async getRoomSettings(workspace, name) {
      const r = await rooms.getRoomSettings(ref(workspace, name));
      return {
        workspace: settingsFrom(r.workspace),
        role: settingsFrom(r.role),
        room: settingsFrom(r.room),
        effective: r.effective ? effectiveFrom(r.effective) : { room: name, role: "", settings: emptySettings(), sources: [] },
        sharedVolumes: mapValues(r.sharedVolumes, (m) => (m === "ro" ? ("ro" as const) : ("rw" as const))),
      };
    },
    async updateRoomSettings(workspace, name, settings) {
      const r = await rooms.updateRoomSettings({ ...ref(workspace, name), settings: settingsTo(settings) });
      return r.effective ? effectiveFrom(r.effective) : { room: name, role: "", settings, sources: [] };
    },

    async listApprovals() {
      const r = await approvals.listApprovals({});
      return r.approvals.filter((a) => a.state === "pending" || a.state === "").map(approvalFrom);
    },
    async *watchApprovals(signal): AsyncGenerator<ApprovalEvent> {
      for await (const r of approvals.watchApprovals({}, { signal })) {
        const a = r.approval;
        if (!a) continue;
        const x = approvalFrom(a);
        if (a.state === "pending" || a.state === "") yield { kind: "requested", approval: x };
        else if (x.answer) yield { kind: "answered", id: a.id, answer: x.answer };
        else yield { kind: "withdrawn", id: a.id, reason: x.withdrawn ?? a.state };
      }
    },
    async getApproval(id) {
      const pending = await approvals.listApprovals({});
      const hit = pending.approvals.find((a) => a.id === id);
      if (hit) return approvalFrom(hit);
      const hist = await approvals.listApprovalHistory({ limit: 500 });
      const old = hist.approvals.find((a) => a.id === id);
      if (!old) throw new ApiError("not_found", "This approval no longer exists.");
      return approvalFrom(old);
    },
    async answerApproval(id, decision, message) {
      await approvals.answerApproval({ id, decision: decisionTo(decision), message });
    },

    async listConversations() {
      const [fl, ib] = await Promise.all([rooms.listRooms({}), inbox.listInbox({ limit: 500 })]);
      const byPeer = new Map<string, pb.InboxMessage[]>();
      for (const m of ib.messages) {
        const p = peerOf(m);
        byPeer.set(p, [...(byPeer.get(p) ?? []), m]);
      }
      return fl.rooms
        .map((r) => {
          const address = r.address || `${r.name}@${r.workspace}`;
          const msgs = (byPeer.get(address) ?? []).sort((a, b) => iso(a.sentAt).localeCompare(iso(b.sentAt)));
          const last = msgs[msgs.length - 1];
          return {
            address,
            displayName: r.displayName || r.name,
            presence: roomFrom(r).presence,
            lastMessage: last && messageFrom(last),
            unread: msgs.filter((m) => m.direction === "in" && !m.read).length,
          };
        })
        .sort((a, b) => (b.lastMessage?.at ?? "").localeCompare(a.lastMessage?.at ?? "") || a.address.localeCompare(b.address));
    },
    async listMessages(address) {
      const workspace = address.split("@")[1] ?? "";
      const r = await inbox.listInbox({ workspace, peer: address, limit: 200 });
      const unread = r.messages.filter((m) => m.direction === "in" && !m.read).map((m) => m.id);
      if (unread.length) void inbox.markRead({ ids: unread }).catch(() => undefined);
      return r.messages.map(messageFrom).sort((a, b) => a.at.localeCompare(b.at));
    },
    async *watchMessages(signal) {
      for await (const r of inbox.watchInbox({}, { signal })) if (r.message) yield messageFrom(r.message);
    },
    async sendMessage(to, body) {
      const [name, workspace = ""] = to.split("@");
      const r = await inbox.sendMessage({ workspace, to: name, body });
      return messageFrom(r.message!);
    },

    async queryAudit(f) {
      const r = await auditSvc.queryAudit({
        actor: f.actor ?? "",
        actions: f.action ? [f.action] : [],
        workspace: f.workspace ?? "",
        since: f.since ? ts(new Date(f.since)) : undefined,
        until: f.until ? ts(new Date(f.until)) : undefined,
        limit: f.limit ?? 500,
      });
      return r.entries.map((e, i) => ({
        id: `${iso(e.at)}-${i}`,
        source: e.source === pb.AuditSource.MESSAGING ? ("messaging" as const) : ("controlCenter" as const),
        at: iso(e.at),
        actor: e.actor,
        action: e.action,
        object: e.object,
        workspace: e.workspace,
        sourceIP: e.sourceIp,
        result: e.result,
        detail: e.detail || (e.before || e.after ? `${e.before} -> ${e.after}` : ""),
      }));
    },
    async getSpend() {
      const r = await spend.getSpend({});
      return {
        available: r.available,
        updatedAt: iso(r.updatedAt),
        rooms: r.rooms.map((x) => ({ workspace: x.workspace, room: x.room, usd: x.spendUsd, budgetUSD: x.budgetUsd, exhausted: x.budgetExhausted })),
        workspaces: r.workspaces.map((w) => ({ workspace: w.workspace, usd: w.spendUsd, models: w.models.map((m) => ({ model: m.model, usd: m.spendUsd })) })),
      };
    },
    async listUsers() {
      const r = await users.listUsers({});
      return r.users.map(
        (u): User => ({
          id: u.id,
          handle: u.handle,
          email: u.email,
          displayName: u.displayName || u.handle,
          role: roleFrom(u.role),
          workspaces: u.workspaces,
          provider: u.provider,
          disabled: u.disabled,
          roleOverride: u.roleOverride,
          lastSeen: iso(u.lastLogin),
        }),
      );
    },
    async updateUser(id, change) {
      if (change.role) await users.setUserRole({ userId: id, role: roleTo(change.role) });
      if (change.workspaces) await users.setUserWorkspaces({ userId: id, workspaces: change.workspaces });
      if (change.disabled !== undefined) await users.setUserDisabled({ userId: id, disabled: change.disabled });
    },
    async getVapidPublicKey() {
      return (await notify.getVapidPublicKey({})).publicKey;
    },
    async subscribePush(sub) {
      await notify.subscribeWebPush({ endpoint: sub.endpoint ?? "", p256dh: sub.keys?.p256dh ?? "", auth: sub.keys?.auth ?? "", userAgent: navigator.userAgent });
    },
    async unsubscribePush(endpoint) {
      await notify.unsubscribeWebPush({ endpoint });
    },
    async getNotificationPrefs() {
      const r = await notify.getNotificationPrefs({});
      return {
        prefs: r.prefs.map((p) => ({ workspace: p.workspace, event: p.event as NotificationEvent, channels: p.channels as ChannelKind[] })),
        availableChannels: r.availableChannels as ChannelKind[],
      };
    },
    async setNotificationPrefs(p) {
      await notify.setNotificationPrefs({ prefs: p.prefs });
    },
    async sendTestNotification(channel) {
      await notify.sendTestNotification({ channel });
    },

    async listChannels() {
      const wss = await myWorkspaces();
      const per = await Promise.all(
        wss.map(async (workspace) => {
          const r = await channels.listChannels({ workspace });
          return r.channels.map((c) => ({
            workspace,
            name: c.name,
            description: c.description,
            access: access(c.access),
            delivery: delivery(c.delivery),
            allowedDelivery: c.allowedDelivery.map(delivery),
            unread: c.unread,
            members: c.members,
          }));
        }),
      );
      return per.flat();
    },
    async readMessages(workspace, scope, opts) {
      const r = await channels.readChannelMessages({ workspace, scope: scopeTo(scope), unreadOnly: opts.unreadOnly, limit: opts.limit, cursor: opts.cursor ?? "", full: true, markRead: false });
      return { items: r.items.map((i) => itemFrom(workspace, i)), nextCursor: r.nextCursor, unreadRemaining: r.unreadRemaining };
    },
    async post(workspace, channel, body) {
      const r = await channels.postToChannel({ workspace, channel: channel.replace(/^#/, ""), body });
      return { id: r.id, mentioned: r.mentioned };
    },
    async setDelivery(workspace, channel, mode) {
      await channels.setChannelDelivery({ workspace, channel: channel.replace(/^#/, ""), mode });
    },
    async markRead(workspace, scope, upTo) {
      await channels.markChannelRead({ workspace, scope: scopeTo(scope), upTo: upTo ?? "" });
    },
    // The server reads messaging for the person every ~2 s and streams new items.
    async *watchItems(signal) {
      for await (const r of channels.watchChannelItems({}, { signal })) if (r.item) yield itemFrom(r.workspace, r.item);
    },
    async mentionCandidates(workspace) {
      const [ms, rs] = await Promise.all([channels.listMentionables({ workspace }), listRooms(workspace).catch(() => [] as pb.RoomSummary[])]);
      const roles = [...new Set(rs.map((r) => r.role).filter(Boolean))];
      const groups = [...new Set(rs.map((r) => r.group).filter(Boolean))];
      return [
        ...ms.mentionables.map(
          (m): MentionCandidate => ({
            handle: m.handle,
            kind: m.kind === "person" ? "person" : "agent",
            detail: [m.kind === "person" ? m.displayName : m.role, m.reachable ? "" : "not reachable from you"].filter(Boolean).join(" · "),
          }),
        ),
        ...roles.map((r) => ({ handle: `role:${r}`, kind: "group" as const, detail: `every ${r}` })),
        ...groups.map((g) => ({ handle: `group:${g}`, kind: "group" as const, detail: `group ${g}` })),
      ];
    },
    async listVerdicts(workspace, room) {
      const r = await warden.listVerdicts({ workspace, room: room ?? "", limit: 200 });
      return r.verdicts.map(verdictFrom);
    },
    async decideVerdict(id, decision) {
      await warden.decideVerdict({ id, decision: decision === "dismissed" ? "dismiss" : "confirm" });
    },
  };
  return withErrors({ ...api, observatory } as Api);
}
