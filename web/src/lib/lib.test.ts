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
import { defaultRoles, defaultRoom, emptyDraft, pickNames, reachability, resizeRooms, sortFleet, validateDraft } from "./workspace";

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
    expect(defaultRoles(4)).toEqual(["coordinator", "implementer", "reviewer", "chronicler"]);
    const six = defaultRoles(6);
    expect(six).toHaveLength(6);
    expect(six).toEqual(expect.arrayContaining(["ci", "qa", "chronicler"]));
  });

  it("resizes rooms keeping existing ones", () => {
    const r = resizeRooms([defaultRoom("zeus", "coordinator", "m")], 3, { names: ["hermes", "metis"], roles: ["coordinator", "implementer", "reviewer"] }, "m");
    expect(r.map((x) => `${x.name}:${x.role}`)).toEqual(["zeus:coordinator", "hermes:implementer", "metis:reviewer"]);
    expect(resizeRooms(r, 1, { names: [], roles: [] }, "m").map((x) => x.name)).toEqual(["zeus"]);
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
    const r = {
      available: true,
      updatedAt: "",
      rooms: [
        { workspace: "w", room: "a", usd: 2, budgetUSD: 10, exhausted: false },
        { workspace: "w", room: "b", usd: 3, budgetUSD: 10, exhausted: false },
        { workspace: "v", room: "c", usd: 1, budgetUSD: 0, exhausted: false },
      ],
      workspaces: [
        { workspace: "w", usd: 5, models: [{ model: "m1", usd: 5 }] },
        { workspace: "v", usd: 1, models: [{ model: "m1", usd: 0.5 }, { model: "m2", usd: 0.5 }] },
      ],
    };
    expect(groupSpend(r, "workspace").map((g) => [g.key, g.usd, g.budgetUSD])).toEqual([["w", 5, 20], ["v", 1, 0]]);
    expect(groupSpend(r, "model")[0]).toMatchObject({ key: "m1", usd: 5.5, budgetUSD: 0 });
    expect(groupSpend(r, "room")[0].key).toBe("b@w");
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
    expect(providerLoginURL({ kind: "oidc", name: "keycloak" }, "/wizard")).toBe("/auth/oidc/keycloak/login?return=%2Fwizard");
    expect(providerLoginURL({ kind: "saml", name: "x", loginURL: "/auth/saml/x/login" })).toBe("/auth/saml/x/login?return=%2F");
  });
});
