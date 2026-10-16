import { describe, expect, it } from "vitest";
import type { RoomSummary, WorkspaceDraft } from "@/api/types";
import { Hub } from "@/api/stream";
import { safeNext } from "@/pages/SignIn";
import { providerLoginURL } from "./auth";
import { toJSONL } from "./download";
import { ago, bytes } from "./format";
import { applyApprovalEvent, applyFleetEvent } from "./live";
import { serviceProgress } from "./platform";
import { urlBase64ToUint8Array } from "./push";
import { groupSpend } from "./spend";
import { defaultRoles, defaultRoom, emptyDraft, estimateFit, pickNames, reachability, resizeRooms, sortFleet, validateDraft } from "./workspace";

const room = (name: string, presence: RoomSummary["presence"], workspace = "acme"): RoomSummary => ({
  workspace,
  name,
  displayName: name,
  role: "implementer",
  group: "",
  presence,
  cli: "claude",
  model: "m",
  currentTask: "",
  spendUSD: 0,
  budgetUSD: 0,
  diskUsedBytes: 0,
  diskCapacityBytes: 1,
  phase: "Running",
  paused: false,
  updateAvailable: false,
  pendingApprovals: 0,
});

describe("names and roles", () => {
  it("skips used names and falls back to agent-N", () => {
    expect(pickNames(3, ["athena"])).toEqual(["hermes", "metis", "apollo"]);
    const all = pickNames(100, []);
    expect(new Set(all).size).toBe(100);
    expect(all.at(-1)).toMatch(/^agent-\d+$/);
  });

  it("builds the default role mix", () => {
    expect(defaultRoles(1)).toEqual(["implementer"]);
    expect(defaultRoles(3)).toEqual(["coordinator", "implementer", "reviewer"]);
    expect(defaultRoles(5)).toEqual(["coordinator", "implementer", "implementer", "implementer", "reviewer"]);
    const six = defaultRoles(6);
    expect(six).toHaveLength(6);
    expect(six).toContain("ci-infra");
    expect(six).toContain("qa");
  });

  it("resizes rooms keeping existing ones", () => {
    const r = resizeRooms([defaultRoom("zeus", "coordinator", "m")], 3, ["athena"], "m");
    expect(r.map((x) => x.name)).toEqual(["zeus", "hermes", "metis"]);
    expect(resizeRooms(r, 1, [], "m").map((x) => x.name)).toEqual(["zeus"]);
  });
});

describe("drafts", () => {
  const draft = (): WorkspaceDraft => ({ ...emptyDraft("m"), name: "acme", org: "acme", rooms: [defaultRoom("athena", "implementer", "m"), defaultRoom("hermes", "reviewer", "m")] });

  it("validates names, duplicates and groups", () => {
    expect(validateDraft(draft())).toEqual([]);
    const d = draft();
    d.name = "Acme";
    d.rooms[1].name = "athena";
    d.rooms[0].group = "ghost";
    const errs = validateDraft(d);
    expect(errs.join("\n")).toMatch(/DNS label/);
    expect(errs.join("\n")).toMatch(/used twice/);
    expect(errs.join("\n")).toMatch(/unknown group "ghost"/);
  });

  it("estimates fit", () => {
    const f = estimateFit(draft().rooms, { cpu: 3, memoryGi: 100, diskGi: 1000, roomEstimate: 1 });
    expect(f.fits).toBe(false);
    expect(f.short).toEqual(["cpu"]);
    expect(f.diskGi).toBe(180);
  });

  it("computes reachability with mediated links", () => {
    const d = draft();
    d.groups = ["clean", "dirty"];
    d.rooms[0].group = "clean";
    d.rooms[1].group = "dirty";
    d.links = [
      { from: "clean", to: "dirty", mode: "mediated", senders: ["athena"], recipients: [], requirePrefix: "to {recipient}:" },
      { from: "dirty", to: "clean", mode: "none", senders: [], recipients: [], requirePrefix: "" },
    ];
    const r = reachability(d);
    expect(r.filter((e) => e.from === e.to).map((e) => e.from)).toEqual(["clean", "dirty"]);
    const cross = r.filter((e) => e.from !== e.to);
    expect(cross).toHaveLength(1);
    expect(cross[0].label).toBe('mediated from athena prefix "to {recipient}:"');
    d.defaultLinks = "none";
    expect(reachability(d).some((e) => e.from === e.to)).toBe(false);
  });
});

describe("live state", () => {
  it("sorts the fleet: waiting, crashed, working, idle, offline", () => {
    const s = sortFleet([room("a", "idle"), room("b", "working"), room("c", "waitingOnHuman"), room("d", "offline"), room("e", "crashed")]);
    expect(s.map((r) => r.name)).toEqual(["c", "e", "b", "a", "d"]);
  });

  it("applies fleet events", () => {
    let rooms = applyFleetEvent([], { kind: "snapshot", rooms: [room("a", "idle")] });
    rooms = applyFleetEvent(rooms, { kind: "upsert", room: room("a", "working") });
    rooms = applyFleetEvent(rooms, { kind: "upsert", room: room("b", "idle") });
    expect(rooms.map((r) => `${r.name}:${r.presence}`)).toEqual(["a:working", "b:idle"]);
    rooms = applyFleetEvent(rooms, { kind: "remove", workspace: "acme", name: "a" });
    expect(rooms.map((r) => r.name)).toEqual(["b"]);
  });

  it("applies approval events", () => {
    const a = { id: "1" } as never;
    let l = applyApprovalEvent([], { kind: "requested", approval: a });
    l = applyApprovalEvent(l, { kind: "requested", approval: a });
    expect(l).toHaveLength(1);
    l = applyApprovalEvent(l, { kind: "withdrawn", id: "1", reason: "x" });
    expect(l).toHaveLength(0);
  });

  it("hub streams initial and published values until aborted", async () => {
    const hub = new Hub<number>();
    const ctl = new AbortController();
    const got: number[] = [];
    const done = (async () => {
      for await (const v of hub.subscribe(ctl.signal, [1])) {
        got.push(v);
        if (got.length === 3) ctl.abort();
      }
    })();
    await Promise.resolve();
    hub.publish(2);
    hub.publish(3);
    await done;
    expect(got).toEqual([1, 2, 3]);
  });
});

describe("platform", () => {
  it("reports service progress from conditions", () => {
    const spec = { services: { ntfyEnabled: true } } as never;
    const p = serviceProgress(spec, [
      { type: "PostgresReady", status: "True", reason: "Ready", message: "" },
      { type: "ForgejoReady", status: "False", reason: "Deploying", message: "pulling image" },
      { type: "MessagingReady", status: "False", reason: "InstallFailed", message: "" },
    ]);
    expect(p.map((x) => x.state)).toEqual(["ready", "progress", "failed", "waiting", "waiting", "waiting"]);
    expect(p[1].message).toBe("pulling image");
  });
});

describe("helpers", () => {
  it("formats", () => {
    expect(bytes(0)).toBe("0 B");
    expect(bytes(1536)).toBe("1.5 KiB");
    expect(bytes(30 * 1024 ** 3)).toBe("30.0 GiB");
    expect(ago(new Date(Date.now() - 5 * 60_000).toISOString())).toBe("5 min ago");
  });

  it("exports JSONL", () => {
    expect(toJSONL([{ a: 1 }, { b: "x" }])).toBe('{"a":1}\n{"b":"x"}\n');
    expect(toJSONL([])).toBe("");
  });

  it("groups spend", () => {
    const rows = [
      { workspace: "w", room: "a", model: "m1", usd: 2, budgetUSD: 10, inputTokens: 1, outputTokens: 1 },
      { workspace: "w", room: "b", model: "m1", usd: 3, budgetUSD: 10, inputTokens: 1, outputTokens: 1 },
      { workspace: "v", room: "c", model: "m2", usd: 1, budgetUSD: 0, inputTokens: 1, outputTokens: 1 },
    ];
    expect(groupSpend(rows, "workspace").map((g) => [g.key, g.usd, g.budgetUSD])).toEqual([["w", 5, 20], ["v", 1, 0]]);
    expect(groupSpend(rows, "model")[0]).toMatchObject({ key: "m1", usd: 5, budgetUSD: 0 });
    expect(groupSpend(rows, "room")[0].key).toBe("b@w");
  });

  it("decodes VAPID keys", () => {
    expect([...urlBase64ToUint8Array("AQID_-8")]).toEqual([1, 2, 3, 255, 239]);
  });

  it("only follows same-origin paths after sign-in", () => {
    expect(safeNext("/approvals/1")).toBe("/approvals/1");
    expect(safeNext("//evil.example")).toBe("/");
    expect(safeNext("https://evil.example")).toBe("/");
    expect(safeNext(undefined)).toBe("/");
  });

  it("builds provider login URLs", () => {
    expect(providerLoginURL({ kind: "oidc", name: "keycloak" }, "/wizard", true)).toBe("/auth/oidc/keycloak/login?next=%2Fwizard&test=1");
  });
});
