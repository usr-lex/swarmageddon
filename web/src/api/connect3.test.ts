import { create } from "@bufbuild/protobuf";
import { createRouterTransport } from "@connectrpc/connect";
import { describe, expect, it } from "vitest";
import * as pb from "@/gen/swarm/controlcenter/v1/controlcenter_pb";
import { createConnectApi, transcriptFrom } from "./connect";
import type { WorkspaceDraft } from "./types";

// Isolation, preflight re-run, transcript, prune, advisors and warden, the
// channel stream and mentionables against an in-memory router.
function fake() {
  const calls: Record<string, unknown> = {};
  const transport = createRouterTransport(({ service }) => {
    service(pb.AuthService, { getMe: () => ({ user: { handle: "lex", role: pb.Role.ADMIN, workspaces: ["acme"] } }) });
    service(pb.PlatformService, {
      setIsolation: (r) => {
        calls.isolation = r;
        return { platform: { isolationTier: r.tier, privilegedAccepted: r.privilegedAccepted, status: { preflightRerunRequested: "t2", preflightRerun: "t1" } } };
      },
      rerunPreflight: () => {
        calls.rerun = true;
        return { requested: "t3" };
      },
    });
    service(pb.RoomService, {
      async *watchTranscript(r) {
        calls.transcript = r;
        yield { item: { id: "a", role: "user", text: "hello" } };
        yield { item: { id: "b", role: "tool", toolName: "Bash", toolInput: "ls" } };
        yield { item: { id: "c", role: "tool_result", toolName: "Bash", toolResult: "go.mod" } };
      },
      pruneEngine: (r) => {
        calls.prune = r;
        return { output: "Total reclaimed space: 1GB", reclaimedBytes: 1_000_000_000n };
      },
      listRooms: () => ({ rooms: [{ workspace: "acme", name: "athena", role: "reviewer", group: "clean" }] }),
    });
    service(pb.WorkspaceService, {
      previewWorkspace: (r) => {
        calls.preview = r.plan;
        return {};
      },
      getWorkspace: () => ({
        plan: {
          name: "acme",
          agents: [
            { name: "athena", role: "reviewer", cli: "claude", advisor: { model: "" }, wardenMode: "gate" },
            { name: "hermes", role: "implementer", cli: "ore", advisor: { model: "gpt-6", effort: "max", maxCallsPerHour: 3, monthlyUsd: 9 } },
          ],
          roleAdvisors: { reviewer: { model: "opus", effort: "high", maxCallsPerHour: 10, monthlyUsd: 30, native: true } },
          warden: { mode: "gate", model: "haiku", interruptOnHigh: false, quarantineOnHigh: true, monthlyUsd: 15 },
        },
      }),
    });
    service(pb.ChannelService, {
      async *watchChannelItems() {
        yield { workspace: "acme", item: { id: "m1", channel: "general", from: "athena@acme", fromKind: "agent", body: "hi @lex", mentionsMe: true } };
      },
      listMentionables: () => ({
        mentionables: [
          { handle: "athena", kind: "agent", role: "Reviewer", reachable: true },
          { handle: "olga", kind: "person", displayName: "Olga", reachable: false },
        ],
      }),
    });
  });
  return { api: createConnectApi(transport), calls };
}

const draft = (): WorkspaceDraft => ({
  name: "acme",
  displayName: "",
  org: "acme",
  defaultModel: "m",
  perRoomMonthlyUSD: 10,
  rooms: [
    { name: "athena", role: "reviewer", cli: "claude", model: "m", effort: "high", group: "", cpu: 2, memoryGi: 6, homeGi: 30, engineGi: 60, repos: [], advisor: null, wardenMode: "gate" },
    { name: "hermes", role: "implementer", cli: "ore", model: "m", effort: "high", group: "", cpu: 2, memoryGi: 6, homeGi: 30, engineGi: 60, repos: [] },
  ],
  groups: [],
  defaultLinks: "withinGroup",
  links: [],
  repositories: [],
  channels: [],
  events: [],
  chatChannels: [],
  storm: { pushPerMinute: 6, coalesceBytes: 4096, digestMinutes: 15, fanoutPerMinute: 60 },
  warden: { mode: "monitor", model: "haiku", interruptOnHigh: false, quarantineOnHigh: true, monthlyUSD: 12 },
  advisors: { reviewer: { model: "opus", effort: "max", maxCallsPerHour: 5, monthlyUSD: 30, native: false }, implementer: { model: "", effort: "high", maxCallsPerHour: 10, monthlyUSD: 0, native: false } },
  roomDefaults: { env: {}, secretEnv: [], mcpServers: {} },
  roleDefaults: {},
  sharedVolumes: [],
});

describe("connect: platform", () => {
  it("sets the isolation tier and re-runs preflight", async () => {
    const { api, calls } = fake();
    const p = await api.updateIsolation("privileged", true);
    expect(calls.isolation).toMatchObject({ tier: "privileged", privilegedAccepted: true });
    expect(p.spec).toMatchObject({ isolationTier: "privileged", privilegedAccepted: true });
    expect(p.preflight.rerunPending).toBe(true);
    await api.rerunPreflight();
    expect(calls.rerun).toBe(true);
  });
});

describe("connect: rooms", () => {
  it("streams the transcript", async () => {
    const { api, calls } = fake();
    const got = [];
    for await (const e of api.watchTranscript("acme", "athena", new AbortController().signal)) got.push(e);
    expect(calls.transcript).toMatchObject({ room: { workspace: "acme", name: "athena" }, tailLines: 200 });
    expect(got.map((e) => [e.kind, e.text])).toEqual([
      ["user", "hello"],
      ["tool", "Bash: ls"],
      ["toolResult", "go.mod"],
    ]);
    expect(transcriptFrom(create(pb.TranscriptItemSchema, { id: "x", role: "weird", text: "?" })).kind).toBe("system");
  });

  it("prunes the engine", async () => {
    const { api, calls } = fake();
    const r = await api.prune("acme", "athena", "buildCache");
    expect(calls.prune).toMatchObject({ room: { workspace: "acme", name: "athena" }, kind: "buildCache" });
    expect(r).toEqual({ reclaimedBytes: 1e9, output: "Total reclaimed space: 1GB" });
  });
});

describe("connect: advisors and warden", () => {
  it("sends advisors, room overrides and the warden", async () => {
    const { api, calls } = fake();
    await api.planWorkspace(draft());
    const plan = calls.preview as pb.WorkspacePlan;
    expect(plan.warden).toMatchObject({ mode: "monitor", model: "haiku", interruptOnHigh: false, quarantineOnHigh: true, monthlyUsd: 12 });
    expect(plan.roleAdvisors.reviewer).toMatchObject({ model: "opus", effort: "max", maxCallsPerHour: 5, monthlyUsd: 30 });
    expect(plan.roleAdvisors.implementer.model).toBe("");
    expect(plan.agents[0].advisor?.model).toBe("");
    expect(plan.agents[0].wardenMode).toBe("gate");
    expect(plan.agents[1].advisor).toBeUndefined();
  });

  it("reads them back", async () => {
    const { api } = fake();
    const d = await api.getWorkspaceDraft("acme");
    expect(d.warden).toEqual({ mode: "gate", model: "haiku", interruptOnHigh: false, quarantineOnHigh: true, monthlyUSD: 15 });
    expect(d.advisors.reviewer).toEqual({ model: "opus", effort: "high", maxCallsPerHour: 10, monthlyUSD: 30, native: true });
    expect(d.rooms[0].advisor).toBeNull();
    expect(d.rooms[0].wardenMode).toBe("gate");
    expect(d.rooms[1].advisor).toMatchObject({ model: "gpt-6", effort: "max", maxCallsPerHour: 3, monthlyUSD: 9 });
  });
});

describe("connect: channels", () => {
  it("streams new items from the server", async () => {
    const { api } = fake();
    const got = [];
    for await (const i of api.watchItems(new AbortController().signal)) got.push(i);
    expect(got).toHaveLength(1);
    expect(got[0]).toMatchObject({ id: "m1", workspace: "acme", channel: "general", mentionsMe: true });
  });

  it("lists mentionables, then role and group addresses", async () => {
    const { api } = fake();
    const c = await api.mentionCandidates("acme");
    expect(c).toEqual([
      { handle: "athena", kind: "agent", detail: "Reviewer" },
      { handle: "olga", kind: "person", detail: "Olga · not reachable from you" },
      { handle: "role:reviewer", kind: "group", detail: "every reviewer" },
      { handle: "group:clean", kind: "group", detail: "group clean" },
    ]);
  });
});
