import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { createRouterTransport } from "@connectrpc/connect";
import { describe, expect, it } from "vitest";
import * as pb from "@/gen/swarm/controlcenter/v1/controlcenter_pb";
import { createConnectApi, durationMinutes, roomFrom, settingsFrom, settingsTo } from "./connect";
import { emptyMcp, type RoomSettings } from "./settings";

// Channels, warden, quarantine and layered settings against an in-memory router.
function fake() {
  const calls: Record<string, unknown> = {};
  const transport = createRouterTransport(({ service }) => {
    service(pb.AuthService, { getMe: () => ({ user: { handle: "lex", role: pb.Role.OPERATOR, workspaces: ["acme"] } }) });
    service(pb.ChannelService, {
      listChannels: (r) => ({ channels: [{ name: `general-${r.workspace}`, access: "readWrite", delivery: "digest", allowedDelivery: ["push", "digest"], unread: 3, members: 5 }] }),
      readChannelMessages: (r) => {
        calls.read = r;
        return {
          items: [{ id: "m1", channel: "general", from: "hermes", fromKind: "agent", body: "hi @lex", sentAt: timestampFromDate(new Date("2026-10-16T10:00:00Z")), mentionsMe: true }],
          nextCursor: "c2",
          unreadRemaining: 4,
        };
      },
      postToChannel: (r) => {
        calls.post = r;
        return { id: "m2", mentioned: ["hermes"] };
      },
      markChannelRead: (r) => {
        calls.mark = r;
        return {};
      },
    });
    service(pb.WardenService, {
      listVerdicts: () => ({
        verdicts: [
          { id: "v1", workspace: "acme", room: "metis", layer: "judge", severity: pb.Severity.HIGH, rule: "model", reason: "pushes to a mirror", action: "interrupted", source: "room" },
          { id: "v2", workspace: "acme", room: "metis", layer: "rules", severity: pb.Severity.LOW, decision: "dismiss", decidedBy: "lex" },
        ],
      }),
      decideVerdict: (r) => {
        calls.decide = r;
        return {};
      },
    });
    service(pb.RoomService, {
      quarantineRoom: (r) => {
        calls.quarantine = r;
        return {};
      },
      releaseRoom: (r) => {
        calls.release = r;
        return {};
      },
      getRoomSettings: () => ({
        workspace: { env: { TZ: "UTC" }, network: { mode: "limited", domains: ["a.example"] } },
        role: {},
        room: { mcpServers: { linear: { type: "http", url: "https://mcp.example", headers: { Authorization: { valueFrom: { name: "room-secrets", key: "linear" } } } } } },
        effective: { room: "metis", role: "reviewer", settings: { env: { TZ: "UTC" } }, sources: [{ kind: "env", key: "TZ", level: "workspace" }] },
        sharedVolumes: { datasets: "ro" },
      }),
    });
    service(pb.WorkspaceService, {
      setRoomSecret: (r) => ({ ref: { name: "room-secrets", key: r.key } }),
      previewWorkspace: (r) => {
        calls.preview = r.plan;
        return { fits: true, effective: [{ room: "hermes", role: "implementer", settings: { network: { mode: "none" } }, sources: [{ kind: "network", key: "mode", level: "room" }] }] };
      },
    });
  });
  return { api: createConnectApi(transport), calls };
}

describe("connect: channels, warden, rooms", () => {
  it("lists channels per workspace and reads with the channel scope", async () => {
    const { api, calls } = fake();
    const chs = await api.listChannels();
    expect(chs).toEqual([expect.objectContaining({ workspace: "acme", name: "general-acme", unread: 3, delivery: "digest" })]);
    const page = await api.readMessages("acme", "#general", { unreadOnly: true, limit: 20 });
    expect((calls.read as pb.ReadChannelMessagesRequest).scope).toBe("general");
    expect((calls.read as pb.ReadChannelMessagesRequest).markRead).toBe(false);
    expect(page.items[0]).toMatchObject({ id: "m1", workspace: "acme", mentionsMe: true, at: "2026-10-16T10:00:00.000Z" });
    expect(page.nextCursor).toBe("c2");
    await api.post("acme", "#general", "hello");
    expect((calls.post as pb.PostToChannelRequest).channel).toBe("general");
    await api.markRead("acme", "mentions");
    expect((calls.mark as pb.MarkChannelReadRequest).scope).toBe("mentions");
  });

  it("maps verdicts and decisions", async () => {
    const { api, calls } = fake();
    const vs = await api.listVerdicts("acme");
    expect(vs[0]).toMatchObject({ severity: "high", layer: "judge", decision: undefined });
    expect(vs[1].decision).toMatchObject({ kind: "dismissed", by: "lex" });
    await api.decideVerdict("v1", "confirmed");
    expect((calls.decide as pb.DecideVerdictRequest).decision).toBe("confirm");
  });

  it("quarantines and releases with a note", async () => {
    const { api, calls } = fake();
    await api.roomAction("acme", "metis", { kind: "quarantine", reason: "odd pushes" });
    expect((calls.quarantine as pb.QuarantineRoomRequest).reason).toBe("odd pushes");
    await api.roomAction("acme", "metis", { kind: "release", note: "reviewed" });
    expect((calls.release as pb.ReleaseRoomRequest).note).toBe("reviewed");
  });

  it("reads layered settings", async () => {
    const { api } = fake();
    const s = await api.getRoomSettings("acme", "metis");
    expect(s.workspace.network).toEqual({ mode: "limited", domains: ["a.example"], packageManagers: undefined });
    expect(s.room.mcpServers.linear.headers.Authorization.valueFrom).toEqual({ name: "room-secrets", key: "linear" });
    expect(s.effective.sources[0].level).toBe("workspace");
    expect(s.sharedVolumes).toEqual({ datasets: "ro" });
    expect(await api.setRoomSecret("acme", "linear", "x")).toEqual({ name: "room-secrets", key: "linear" });
  });

  it("sends channels, storm, warden and settings in the plan, and returns effective settings", async () => {
    const { api, calls } = fake();
    const res = await api.planWorkspace({
      name: "acme",
      displayName: "",
      org: "acme",
      defaultModel: "m",
      perRoomMonthlyUSD: 10,
      rooms: [{ name: "hermes", role: "implementer", cli: "claude", model: "m", effort: "high", group: "", cpu: 2, memoryGi: 6, homeGi: 30, engineGi: 60, repos: [], settings: { env: {}, secretEnv: [], mcpServers: {}, network: { mode: "none", domains: [] } } }],
      groups: [],
      defaultLinks: "withinGroup",
      links: [],
      repositories: [],
      channels: [],
      events: [],
      chatChannels: [{ name: "general", description: "", members: [{ select: "everyone", access: "readWrite" }], defaultDelivery: "digest", allowedDelivery: [], postsPerMinute: 20 }],
      storm: { pushPerMinute: 6, coalesceBytes: 4096, digestMinutes: 15, fanoutPerMinute: 60 },
      warden: { mode: "gate", model: "haiku", interruptOnHigh: true, quarantineOnHigh: false, monthlyUSD: 20 },
      advisors: {},
      roomDefaults: { env: { TZ: "UTC" }, secretEnv: [], mcpServers: {} },
      roleDefaults: { reviewer: { env: {}, secretEnv: [], mcpServers: { gh: { ...emptyMcp(), command: "gh-mcp" } } } },
      sharedVolumes: [{ name: "data", sizeGi: 20, storageClass: "", mounts: [{ select: "everyone", readOnly: true }] }],
    });
    const plan = calls.preview as pb.WorkspacePlan;
    expect(plan.channels[0].name).toBe("general");
    expect(plan.storm?.digestInterval).toBe("15m");
    expect(plan.wardenMode).toBe("gate");
    expect(plan.roomDefaults?.env).toEqual({ TZ: "UTC" });
    expect(plan.roleDefaults.reviewer.mcpServers.gh.command).toBe("gh-mcp");
    expect(plan.sharedVolumes[0].size).toBe("20Gi");
    expect(plan.agents[0].settings?.network?.mode).toBe("none");
    expect(res.effective[0]).toMatchObject({ room: "hermes", settings: { network: { mode: "none" } } });
  });
});

describe("connect: helpers", () => {
  it("parses durations", () => {
    expect(durationMinutes("15m")).toBe(15);
    expect(durationMinutes("1h30m")).toBe(90);
    expect(durationMinutes("90s")).toBe(1.5);
  });

  it("round-trips settings", () => {
    const s: RoomSettings = {
      env: { A: "1" },
      secretEnv: [{ name: "TOKEN", secret: { name: "room-secrets", key: "t" } }],
      mcpServers: { x: { ...emptyMcp(), command: "x", env: { K: { value: "", valueFrom: { name: "room-secrets", key: "k" } } } } },
      network: { mode: "limited", domains: ["a.example"], packageManagers: false },
    };
    expect(settingsFrom(settingsTo(s))).toEqual(s);
  });

  it("reads quarantine and warden severity from room summaries", () => {
    const r = roomFrom(create(pb.RoomSummarySchema, { name: "m", quarantined: true, quarantineReason: "why", wardenSeverity: pb.Severity.CRITICAL, undecidedVerdicts: 2 }));
    expect(r).toMatchObject({ quarantined: true, quarantineReason: "why", verdict: "critical" });
    expect(roomFrom(create(pb.RoomSummarySchema, { name: "m", wardenSeverity: pb.Severity.HIGH })).verdict).toBeUndefined();
  });
});
