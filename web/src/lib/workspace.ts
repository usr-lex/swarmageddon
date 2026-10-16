import type { GroupLink, RoomDraft, WorkspaceDraft, Presence, RoomSummary } from "@/api/types";

/** Default agent names (§8.3): Greek gods, in the order they're offered. */
export const greekGods = [
  "athena", "hermes", "metis", "apollo", "artemis", "hephaestus", "hestia", "demeter",
  "proteus", "hecate", "iris", "nike", "themis", "mnemosyne", "eos", "helios", "selene",
  "hyperion", "rhea", "cronus", "ares", "dionysus", "persephone", "hades", "poseidon",
  "zeus", "hera", "aphrodite", "pan", "nyx", "tyche", "nemesis", "asclepius", "hygieia",
  "morpheus", "hypnos", "thanatos", "eris", "harmonia", "boreas", "zephyrus", "notus",
];

/** Room names are DNS labels: lower case letters, digits and dashes. */
export const roomNamePattern = /^[a-z]([a-z0-9-]{0,30}[a-z0-9])?$/;

/** Picks n names from the pool, skipping the used ones; falls back to agent-<i>. */
export function pickNames(n: number, used: Iterable<string>): string[] {
  const taken = new Set(used);
  const out: string[] = [];
  for (const g of greekGods) {
    if (out.length >= n) break;
    if (!taken.has(g)) {
      out.push(g);
      taken.add(g);
    }
  }
  for (let i = 1; out.length < n; i++) {
    const name = `agent-${i}`;
    if (!taken.has(name)) {
      out.push(name);
      taken.add(name);
    }
  }
  return out;
}

/**
 * The default role mix for n agents (§8.3, §8.7): one coordinator, one
 * reviewer, implementers for the rest, a chronicler from four agents up, and
 * CI and QA past five. The server's SuggestAgentNames is authoritative; this
 * is the mock's copy.
 */
export function defaultRoles(n: number): string[] {
  if (n <= 0) return [];
  if (n === 1) return ["implementer"];
  if (n === 2) return ["coordinator", "implementer"];
  if (n === 3) return ["coordinator", "implementer", "reviewer"];
  const extras = n > 5 ? ["ci", "qa"] : [];
  const implementers = n - 3 - extras.length;
  return ["coordinator", ...Array<string>(implementers).fill("implementer"), "reviewer", ...extras, "chronicler"];
}

export const defaultRoom = (name: string, role: string, model: string): RoomDraft => ({
  name,
  role,
  cli: "claude",
  model,
  effort: "high",
  group: "",
  cpu: 2,
  memoryGi: 6,
  homeGi: 30,
  engineGi: 60,
  repos: [],
});

/**
 * Resizes the room list to n, keeping existing rooms. `suggested` is
 * SuggestAgentNames(n, current names): fresh names, and the role mix for n.
 */
export function resizeRooms(rooms: RoomDraft[], n: number, suggested: { names: string[]; roles: string[] }, model: string): RoomDraft[] {
  if (n <= rooms.length) return rooms.slice(0, n);
  const added = [];
  for (let i = rooms.length; i < n; i++) {
    added.push(defaultRoom(suggested.names[i - rooms.length] ?? `agent-${i + 1}`, suggested.roles[i] ?? "implementer", model));
  }
  return [...rooms, ...added];
}

export function emptyDraft(model: string): WorkspaceDraft {
  return {
    name: "",
    displayName: "",
    org: "",
    defaultModel: model,
    perRoomMonthlyUSD: 200,
    rooms: [],
    groups: [],
    defaultLinks: "withinGroup",
    links: [],
    repositories: [],
    channels: [{ kind: "webPush", target: "" }],
    events: ["approvalWaiting", "idleWithOpenWork", "ciRed", "budget", "diskLow", "roomCrashed"],
  };
}

/** Validation messages for a draft, keyed by section. Empty means valid. */
export function validateDraft(d: WorkspaceDraft): string[] {
  const errs: string[] = [];
  if (!roomNamePattern.test(d.name)) errs.push("Workspace name must be a lower-case DNS label.");
  if (!d.org) errs.push("Forgejo org is required.");
  if (d.rooms.length === 0) errs.push("Add at least one agent.");
  const seen = new Set<string>();
  for (const r of d.rooms) {
    if (!roomNamePattern.test(r.name)) errs.push(`Agent name "${r.name}" must be a lower-case DNS label.`);
    if (seen.has(r.name)) errs.push(`Agent name "${r.name}" is used twice.`);
    seen.add(r.name);
    if (r.group && !d.groups.includes(r.group)) errs.push(`Agent ${r.name} is in unknown group "${r.group}".`);
  }
  for (const l of d.links) {
    for (const g of [l.from, l.to]) if (g && !d.groups.includes(g)) errs.push(`Link uses unknown group "${g}".`);
  }
  return errs;
}

export interface Reach {
  from: string;
  to: string;
  mode: "open" | "mediated";
  label: string;
}

/**
 * Who can reach whom, group to group, for the diagram. "" is the default
 * group (rooms without a group). Within-group reach comes from defaultLinks.
 */
export function reachability(d: WorkspaceDraft): Reach[] {
  const groups = groupsInUse(d);
  const out: Reach[] = [];
  if (d.defaultLinks === "withinGroup") {
    for (const g of groups) out.push({ from: g, to: g, mode: "open", label: "within group" });
  }
  for (const l of d.links) {
    if (l.mode === "none") continue;
    out.push({ from: l.from, to: l.to, mode: l.mode, label: linkLabel(l) });
  }
  return out;
}

export function groupsInUse(d: WorkspaceDraft): string[] {
  const set = new Set(d.groups);
  if (d.rooms.some((r) => !r.group) || set.size === 0) set.add("");
  return [...set];
}

function linkLabel(l: GroupLink): string {
  if (l.mode === "open") return "open";
  const parts = ["mediated"];
  if (l.senders.length) parts.push(`from ${l.senders.join(", ")}`);
  if (l.recipients.length) parts.push(`to ${l.recipients.join(", ")}`);
  if (l.requirePrefix) parts.push(`prefix "${l.requirePrefix}"`);
  return parts.join(" ");
}

const presenceOrder: Record<Presence, number> = { waitingOnHuman: 0, crashed: 1, working: 2, idle: 3, offline: 4 };

/** Fleet order (§8.4): waiting on a human, crashed, working, idle, then offline; by name within each. */
export function sortFleet(rooms: RoomSummary[]): RoomSummary[] {
  return [...rooms].sort(
    (a, b) =>
      presenceOrder[a.presence] - presenceOrder[b.presence] ||
      a.workspace.localeCompare(b.workspace) ||
      a.name.localeCompare(b.name),
  );
}
