import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";
import { describe, expect, it } from "vitest";
import * as pb from "@/gen/swarm/controlcenter/v1/controlcenter_pb";
import { ApiError } from "./api";
import { createConnectApi, parseCPU, parseGi, parseSMTP, roomFrom } from "./connect";

const room = (name: string, extra: Omit<MessageInitShape<typeof pb.RoomSummarySchema>, "$typeName"> = {}) =>
  create(pb.RoomSummarySchema, { workspace: "acme", name, role: "implementer", group: "clean", cli: "claude", phase: "Running", presence: pb.Presence.WORKING, ...extra });

function fake() {
  let applied: pb.WorkspacePlan | undefined;
  const transport = createRouterTransport(({ service }) => {
    service(pb.AuthService, {
      getMe: () => {
        throw new ConnectError("no session", Code.Unauthenticated);
      },
    });
    service(pb.RoomService, {
      listRooms: () => ({ rooms: [room("hermes"), room("metis", { group: "dirty", crashCount: 3 })] }),
    });
    service(pb.WorkspaceService, {
      getWorkspace: () => ({
        plan: {
          name: "acme",
          org: "acme",
          owners: ["alice"],
          requiredApprovals: 2,
          agents: [{ name: "hermes", role: "implementer", cli: "ore", cpu: "500m", memory: "6Gi", homeVolume: "30Gi", engineVolume: "1Ti", workDir: "work/api" }],
          notificationChannels: [{ kind: "slack", secretName: "notify-acme-slack" }],
          repositories: [{ name: "api", pushMirror: "https://github.com/acme/api.git", mirrorSecretName: "repo-api-mirror-abc" }],
          recordKeystrokes: true,
        },
      }),
      applyWorkspace: (req) => {
        applied = req.plan;
        return {};
      },
    });
    service(pb.ObservatoryService, {
      edges: () => ({
        edges: [
          { from: "hermes@acme", to: "metis@acme", kind: pb.EdgeKind.CROSSING, count: 4n },
          { from: "lex@acme", to: "hermes", kind: pb.EdgeKind.HUMAN, count: 2n },
          { from: "hermes", to: "metis", kind: pb.EdgeKind.FORGE_HANDOFF, subkind: "review_requested", count: 1n },
        ],
      }),
      throughput: () => ({
        days: [
          { day: "2026-10-16", actor: "hermes", merged: 2n, ciFailures: 1n, tokens: 1000n },
          { day: "2026-10-16", actor: "metis", merged: 1n, ciFailures: 0n, tokens: 500n },
        ],
        reviewLatency: [{ day: "2026-10-16", medianSeconds: 5400, reviews: 3n }],
        spend: [{ workspace: "acme", room: "hermes", spendUsd: 12.5 }],
      }),
      reports: () => ({ reports: [{ id: 7n, sender: "iris@acme", body: "## Hi", at: timestampFromDate(new Date("2026-10-16T12:00:00Z")) }] }),
    });
  });
  return { api: createConnectApi(transport), applied: () => applied };
}

describe("connect api", () => {
  it("maps Connect errors to ApiError codes", async () => {
    const { api } = fake();
    const err = await api.me().catch((e) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err.code).toBe("unauthenticated");
  });

  it("round-trips a workspace plan, keeping fields the wizard doesn't edit", async () => {
    const f = fake();
    const d = await f.api.getWorkspaceDraft("acme");
    expect(d.rooms[0]).toMatchObject({ name: "hermes", cli: "ore", cpu: 0.5, memoryGi: 6, engineGi: 1024 });
    d.rooms[0].model = "claude-opus-5-5";
    d.channels = [{ kind: "slack", target: "" }];
    await f.api.applyWorkspace(d);
    const p = f.applied()!;
    expect(p.owners).toEqual(["alice"]);
    expect(p.requiredApprovals).toBe(2);
    expect(d.recordKeystrokes).toBe(true);
    expect(p.recordKeystrokes).toBe(true);
    expect(p.agents[0]).toMatchObject({ model: "claude-opus-5-5", workDir: "work/api", cpu: "0.5", engineVolume: "1024Gi" });
    // An empty webhook keeps the stored secret.
    expect(p.notificationChannels[0]).toMatchObject({ secretName: "notify-acme-slack", webhookUrl: "" });
    // So does an empty mirror token; a new one and an import credential are sent.
    expect(p.repositories[0]).toMatchObject({ name: "api", mirrorSecretName: "repo-api-mirror-abc", mirrorToken: "" });
    expect(d.repositories[0].mirrorToken).toBe("");
    d.repositories[0].mirrorToken = "ghp_new";
    d.repositories.push({ name: "web", importURL: "https://github.com/acme/web.git", importCredential: "bot:ghp_x", group: "", pushMirror: "", mirrorToken: "" });
    await f.api.applyWorkspace(d);
    expect(f.applied()!.repositories).toMatchObject([
      { name: "api", mirrorSecretName: "repo-api-mirror-abc", mirrorToken: "ghp_new" },
      { name: "web", importUrl: "https://github.com/acme/web.git", importCredential: "bot:ghp_x" },
    ]);
  });

  it("joins observatory edges with rooms", async () => {
    const { api } = fake();
    const e = await api.observatory.edges("acme", "24h");
    expect(e.messages).toEqual([
      { from: "hermes", to: "metis", kind: "mediated", count: 4 },
      { from: "@lex", to: "hermes", kind: "human", count: 2 },
    ]);
    expect(e.handoffs).toEqual([{ from: "hermes", to: "metis", kind: "reviewRequested", count: 1 }]);
    expect(e.humans).toEqual(["@lex"]);
    expect(e.agents.find((a) => a.name === "metis")).toMatchObject({ presence: "crashed", activity: 5 });
  });

  it("groups throughput by day", async () => {
    const { api } = fake();
    const t = await api.observatory.throughput("acme", 30);
    expect(t.days).toEqual([{ day: "2026-10-16", prsMerged: { hermes: 2, metis: 1 }, tokens: { hermes: 1000, metis: 500 }, ciFailures: 1, reviewLatencyHours: 1.5 }]);
    expect(t.spendUSD).toEqual({ hermes: 12.5 });
    expect(await api.observatory.latestReport("acme")).toMatchObject({ id: "7", author: "iris@acme", markdown: "## Hi" });
  });
});

describe("helpers", () => {
  it("parses quantities", () => {
    expect(parseGi("512Mi")).toBe(0.5);
    expect(parseGi("2Ti")).toBe(2048);
    expect(parseCPU("250m")).toBe(0.25);
    expect(parseCPU("4")).toBe(4);
  });

  it("parses SMTP targets", () => {
    expect(parseSMTP("smtp://bot:p%40ss@mail.example.com:2525?from=a@x&to=b@x,c@x")).toEqual({
      smtpHost: "mail.example.com",
      smtpPort: 2525,
      smtpUsername: "bot",
      smtpPassword: "p@ss",
      emailFrom: "a@x",
      emailTo: "b@x,c@x",
    });
    expect(parseSMTP("not a url")).toBeNull();
  });

  it("derives crashed and disk from a room summary", () => {
    const r = roomFrom(room("x", { crashCount: 2, volumes: [{ name: "home", usedBytes: 5n, capacityBytes: 10n }, { name: "engine", usedBytes: 1n, capacityBytes: 10n }] }));
    expect(r.presence).toBe("crashed");
    expect([r.diskUsedBytes, r.diskCapacityBytes]).toEqual([6, 20]);
  });
});
