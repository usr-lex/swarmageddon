// Realistic generated observatory data for the mock API: a ten-agent swarm
// with a wall (mediated links) between a clean and a dirty group. Seeded, so
// screenshots and tests are stable; the timeline keeps moving live.

import { Hub } from "./stream";
import type { ActivityEvent, ActivityKind, AgentAxes, EdgeKind, HandoffEdge, HeatCell, MessageEdge, MessageMeta, ObservatoryApi, ObsWindow, Report, ThroughputDay } from "./observatory";
import type { Presence, RoomSummary } from "./types";

export interface SwarmAgent {
  name: string;
  group: string;
  role: string;
}

/** The demo swarm. hermes and proteus may cross the wall to metis; hecate back to them. */
export const demoSwarm: SwarmAgent[] = [
  { name: "athena", group: "clean", role: "coordinator" },
  { name: "hermes", group: "clean", role: "implementer" },
  { name: "proteus", group: "clean", role: "implementer" },
  { name: "apollo", group: "clean", role: "reviewer" },
  { name: "artemis", group: "clean", role: "qa" },
  { name: "iris", group: "clean", role: "chronicler" },
  { name: "metis", group: "dirty", role: "coordinator" },
  { name: "hecate", group: "dirty", role: "implementer" },
  { name: "hephaestus", group: "dirty", role: "implementer" },
  { name: "nyx", group: "dirty", role: "reviewer" },
];

export const demoLinks = [
  { from: "clean", to: "dirty", senders: ["hermes", "proteus"], recipients: ["metis"] },
  { from: "dirty", to: "clean", senders: ["hecate"], recipients: ["hermes", "proteus"] },
];

const DAY = 86_400_000;
const HOUR = 3_600_000;
const windowMs: Record<ObsWindow, number> = { "1h": HOUR, "24h": DAY, "7d": 7 * DAY, "30d": 30 * DAY };
const windowScale: Record<ObsWindow, number> = { "1h": 0.06, "24h": 1, "7d": 6.2, "30d": 24 };

/** mulberry32: small, fast, seeded. */
export function rng(seed: number) {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

function hash(s: string): number {
  let h = 2166136261;
  for (let i = 0; i < s.length; i++) h = Math.imul(h ^ s.charCodeAt(i), 16777619);
  return h >>> 0;
}

const day = (t: number) => new Date(t).toISOString().slice(0, 10);

/** Base messages per day between two parties. */
function pairBase(a: SwarmAgent, b: SwarmAgent): number {
  if (a.group !== b.group) return 0;
  if (a.role === "chronicler" || b.role === "chronicler") return b.role === "chronicler" ? 2 : 1;
  if (a.role === "coordinator") return b.role === "implementer" ? 9 : 5;
  if (b.role === "coordinator") return a.role === "implementer" ? 7 : 4;
  if (b.role === "reviewer" && a.role === "implementer") return 5;
  if (a.role === "reviewer" && b.role === "implementer") return 4;
  if (a.role === "qa" || b.role === "qa") return 2;
  return 1.5;
}

const crossings: [string, string, number][] = [
  ["hermes", "metis", 3],
  ["proteus", "metis", 2],
  ["hecate", "hermes", 2.5],
  ["hecate", "proteus", 1.5],
];

const humanPairs: [string, string, number][] = [
  ["@lex", "athena", 3],
  ["athena", "@lex", 2.5],
  ["@lex", "metis", 1.5],
  ["metis", "@lex", 1.2],
  ["@alice", "apollo", 1],
  ["iris", "@lex", 0.5],
];

const roleProfile: Record<string, Omit<AgentAxes, "name" | "role">> = {
  coordinator: { commits: 6, pullRequests: 4, reviews: 10, issues: 28, messages: 60 },
  implementer: { commits: 70, pullRequests: 14, reviews: 6, issues: 5, messages: 22 },
  reviewer: { commits: 8, pullRequests: 3, reviews: 46, issues: 6, messages: 18 },
  qa: { commits: 18, pullRequests: 5, reviews: 9, issues: 22, messages: 12 },
  chronicler: { commits: 0, pullRequests: 0, reviews: 0, issues: 2, messages: 16 },
};

const tasks: Record<string, string[]> = {
  coordinator: ["Splitting the billing epic into task cards", "Triaging new issues", "Rebalancing work after CI outage"],
  implementer: ["Paginate /v1/orders", "Port invoice renderer to streaming", "Fix flaky checkout test", "Add idempotency keys to payments"],
  reviewer: ["Reviewing #142 (orders pagination)", "Reviewing #139 (invoice streaming)"],
  qa: ["End-to-end run of checkout on staging", "Writing regression test for #131"],
  chronicler: ["Drafting the 16:00 status report"],
};

const repoMap: Record<string, string[]> = { clean: ["acme/api", "acme/web"], dirty: ["acme/scraper", "acme/datasets"] };
const repos = (group: string) => repoMap[group] ?? ["acme/api", "acme/web"];

const prTitles = [
  "Paginate /v1/orders",
  "Stream invoice PDFs",
  "Idempotency keys for payments",
  "Fix flaky checkout test",
  "Retry webhooks with backoff",
  "Cache product images",
  "Bump Go to 1.26",
  "Normalize dataset schema",
  "Scraper: respect robots.txt",
  "Dedup crawled pages",
];

const reportsSeed = (ws: string, t0: number): Report[] => [
  {
    id: "R3",
    at: new Date(t0 - 25 * 60_000).toISOString(),
    author: `iris@${ws}`,
    markdown: `## Status, ${new Date(t0 - 25 * 60_000).toUTCString().slice(17, 22)} UTC

**Shipped since the last report**
- \`acme/api\` #142 *Paginate /v1/orders* merged (hermes; reviewed by apollo in 38 min).
- \`acme/web\` #88 *Order history infinite scroll* merged (proteus).

**In flight**
- hermes: idempotency keys for payments (#147, draft, CI green).
- proteus: invoice PDF streaming (#139, changes requested by apollo).
- artemis: checkout end-to-end on staging; two failures filed as #150 and #151.

**Blocked or waiting on a human**
- **hermes** is waiting for approval to run \`git push --force-with-lease\` on #147.
- metis asked **@lex** whether the scraper may store raw HTML (no answer for 3 h).

**Risks**
- Review latency doubled today (median 1.9 h): apollo is the only clean-side reviewer.
- Two CI failures on \`main\` of \`acme/scraper\`, both timeouts in the crawler tests.
`,
  },
  {
    id: "R2",
    at: new Date(t0 - 4 * HOUR - 25 * 60_000).toISOString(),
    author: `iris@${ws}`,
    markdown: `## Status

**Shipped:** #140 *Retry webhooks with backoff* (proteus).

**In flight:** orders pagination (hermes, in review), invoice streaming (proteus).

**Waiting on a human:** nothing.

**Risks:** none new.
`,
  },
  {
    id: "R1",
    at: new Date(t0 - 8 * HOUR - 25 * 60_000).toISOString(),
    author: `iris@${ws}`,
    markdown: `## Status

Quiet night. **3 PRs merged**, CI green on every default branch. athena opened the billing epic (#130) and split it into six cards.
`,
  },
];

/**
 * `rooms` gives the mock's live rooms; a workspace's swarm is its rooms (the
 * demo workspace's rooms are demoSwarm).
 */
export function createMockObservatory(rooms: () => RoomSummary[]): ObservatoryApi {
  const t0 = Date.now();
  const swarm = demoSwarm;
  const swarmOf = (ws: string): SwarmAgent[] =>
    rooms()
      .filter((r) => r.workspace === ws)
      .map((r) => ({ name: r.name, group: r.group || "main", role: r.role }));
  const timelineHub = new Hub<ActivityEvent>();
  const presenceOf = (ws: string, name: string): Presence => rooms().find((r) => r.workspace === ws && r.name === name)?.presence ?? "offline";
  const reports = new Map<string, Report[]>();
  const getReports = (ws: string) => {
    if (!reports.has(ws)) reports.set(ws, reportsSeed(ws, t0));
    return reports.get(ws)!;
  };

  const messageEdges = (w: ObsWindow, swarm: SwarmAgent[]): MessageEdge[] => {
    const known = new Set(swarm.map((a) => a.name));
    const out: MessageEdge[] = [];
    const add = (from: string, to: string, kind: EdgeKind, base: number) => {
      const r = rng(hash(`${from}>${to}/${w}`))();
      const count = Math.round(base * windowScale[w] * (0.6 + 0.8 * r));
      if (count > 0) out.push({ from, to, kind, count });
    };
    for (const a of swarm) for (const b of swarm) if (a !== b) add(a.name, b.name, "open", pairBase(a, b));
    for (const [f, t, n] of crossings) if (known.has(f) && known.has(t)) add(f, t, "mediated", n);
    for (const [f, t, n] of humanPairs) if (known.has(f) || known.has(t)) add(f, t, "human", n);
    return out;
  };

  const handoffEdges = (w: ObsWindow, swarm: SwarmAgent[]): HandoffEdge[] => {
    const out: HandoffEdge[] = [];
    for (const a of swarm) {
      for (const b of swarm) {
        if (a === b || a.group !== b.group) continue;
        let kind: HandoffEdge["kind"] | null = null;
        let base = 0;
        if (a.role === "implementer" && b.role === "reviewer") [kind, base] = ["reviewRequested", 3];
        else if (a.role === "coordinator" && (b.role === "implementer" || b.role === "qa")) [kind, base] = ["assigned", 2];
        else if (a.role === "qa" && b.role === "implementer") [kind, base] = ["mentioned", 1];
        if (!kind) continue;
        const count = Math.round(base * windowScale[w] * (0.6 + 0.8 * rng(hash(`h${a.name}${b.name}${w}`))()));
        if (count > 0) out.push({ from: a.name, to: b.name, kind, count });
      }
    }
    return out;
  };

  const kindsFor: Record<string, ActivityKind[]> = {
    coordinator: ["message", "message", "issueOpened", "comment", "issueClosed", "review"],
    implementer: ["commit", "commit", "commit", "prOpened", "message", "ciPassed", "prMerged", "ciFailed"],
    reviewer: ["review", "review", "review", "comment", "message", "prMerged"],
    qa: ["issueOpened", "commit", "comment", "ciFailed", "message"],
    chronicler: ["message"],
  };

  const makeEvent = (a: SwarmAgent, at: number, r: () => number): ActivityEvent => {
    const kinds = kindsFor[a.role] ?? ["message"];
    let kind = kinds[Math.floor(r() * kinds.length)];
    const repo = repos(a.group)[Math.floor(r() * 2)];
    const n = 120 + Math.floor(r() * 35);
    const title = prTitles[Math.floor(r() * prTitles.length)];
    const peers = swarm.filter((p) => p.group === a.group && p !== a);
    let peer = peers[Math.floor(r() * peers.length)]?.name ?? "athena";
    if (kind === "message" && a.name === "hermes" && r() < 0.25) {
      kind = "crossing";
      peer = "metis";
    }
    const summary: Record<ActivityKind, string> = {
      message: `message to ${peer}`,
      crossing: `crossed the wall to ${peer} (mediated)`,
      refusal: `message to ${peer} refused by policy`,
      commit: `pushed ${1 + Math.floor(r() * 3)} commit(s) to ${title.toLowerCase().replace(/[^a-z]+/g, "-").slice(0, 24)}`,
      prOpened: `opened #${n} ${title}`,
      prMerged: `merged #${n} ${title}`,
      prClosed: `closed #${n}`,
      review: `${r() < 0.7 ? "approved" : "requested changes on"} #${n} ${title}`,
      issueOpened: `opened issue #${n} ${title}`,
      issueClosed: `closed issue #${n}`,
      comment: `commented on #${n}`,
      ciFailed: `CI failed on #${n} (tests)`,
      ciPassed: `CI passed on #${n}`,
      presence: `is now working`,
    };
    return {
      id: `E${at.toString(36)}${Math.floor(r() * 1e6).toString(36)}`,
      at: new Date(at).toISOString(),
      actor: a.name,
      kind,
      target: kind === "message" || kind === "crossing" ? peer : `#${n}`,
      repo: kind === "message" || kind === "crossing" ? "" : repo,
      summary: summary[kind],
    };
  };

  const history = (() => {
    const r = rng(42);
    const out: ActivityEvent[] = [];
    for (let i = 0; i < 260; i++) {
      const a = swarm[Math.floor(r() * swarm.length)];
      out.push(makeEvent(a, t0 - Math.floor(r() * 2 * DAY), r));
    }
    return out.sort((x, y) => y.at.localeCompare(x.at));
  })();

  // Live: a new event every couple of seconds.
  const live = rng(7);
  setInterval(() => {
    const a = swarm[Math.floor(live() * swarm.length)];
    const e = makeEvent(a, Date.now(), live);
    history.unshift(e);
    if (history.length > 1000) history.pop();
    timelineHub.publish(e);
  }, 2500);

  const sleep = (ms: number) => new Promise((res) => setTimeout(res, ms));

  return {
    async edges(ws, w) {
      await sleep(30);
      const swarm = swarmOf(ws);
      const messages = messageEdges(w, swarm);
      const handoffs = handoffEdges(w, swarm);
      const activity = new Map<string, number>();
      for (const e of [...messages, ...handoffs]) {
        activity.set(e.from, (activity.get(e.from) ?? 0) + e.count);
        activity.set(e.to, (activity.get(e.to) ?? 0) + e.count);
      }
      return {
        agents: swarm.map((a) => ({ name: a.name, group: a.group, role: a.role, presence: presenceOf(ws, a.name), activity: activity.get(a.name) ?? 0 })),
        humans: [...new Set(messages.flatMap((m) => [m.from, m.to]).filter((p) => p.startsWith("@")))],
        messages,
        handoffs,
      };
    },
    async pairMessages(ws, a, b, w) {
      const edges = messageEdges(w, swarmOf(ws)).filter((e) => (e.from === a && e.to === b) || (e.from === b && e.to === a));
      const r = rng(hash(`${a}|${b}|${w}`));
      const out: MessageMeta[] = [];
      for (const e of edges) {
        for (let i = 0; i < Math.min(e.count, 40); i++) {
          out.push({ id: `M${hash(`${e.from}${e.to}${i}`).toString(36)}`, at: new Date(t0 - r() * windowMs[w]).toISOString(), from: e.from, to: e.to, kind: e.kind, bytes: 120 + Math.floor(r() * 2400) });
        }
      }
      return out.sort((x, y) => y.at.localeCompare(x.at));
    },
    async now(ws) {
      const r = rng(hash(ws));
      return swarmOf(ws).map((a) => {
        const presence = presenceOf(ws, a.name);
        const options = tasks[a.role] ?? [""];
        const n = 120 + Math.floor(r() * 35);
        return {
          name: a.name,
          role: a.role,
          group: a.group,
          presence,
          currentTask: presence === "offline" ? "" : rooms().find((x) => x.name === a.name)?.currentTask || options[Math.floor(r() * options.length)],
          lastTouched: a.role === "chronicler" ? undefined : { kind: r() < 0.7 ? "pr" : "issue", repo: repos(a.group)[Math.floor(r() * 2)], number: n, title: prTitles[n % prTitles.length] },
          since: new Date(t0 - Math.floor(r() * 5 * HOUR)).toISOString(),
        };
      });
    },
    async heatmap(ws, days) {
      const out: HeatCell[] = [];
      const start = t0 - (days - 1) * DAY;
      for (const a of swarmOf(ws)) {
        const r = rng(hash(`heat${a.name}`));
        const base = { coordinator: 9, implementer: 14, reviewer: 10, qa: 6, chronicler: 3 }[a.role] ?? 5;
        // The swarm started about 120 days ago; dirty side a month later.
        const born = t0 - (a.group === "clean" ? 120 : 90) * DAY;
        for (let t = start; t <= t0; t += DAY) {
          if (t < born) continue;
          const dow = new Date(t).getUTCDay();
          const weekend = dow === 0 || dow === 6 ? 0.45 : 1;
          const ramp = Math.min(1, (t - born) / (21 * DAY));
          const burst = r() < 0.06 ? 2.2 : 1;
          const count = Math.round(base * weekend * ramp * burst * (0.3 + r() * 1.1));
          out.push({ day: day(t), agent: a.name, count });
        }
      }
      return out;
    },
    async axes(ws, w) {
      const scale = windowScale[w] / windowScale["7d"];
      return swarmOf(ws).map((a) => {
        const p = roleProfile[a.role] ?? roleProfile.implementer;
        const r = rng(hash(`axes${a.name}`));
        const j = () => 0.7 + r() * 0.6;
        return {
          name: a.name,
          role: a.role,
          commits: Math.round(p.commits * j() * scale),
          pullRequests: Math.round(p.pullRequests * j() * scale),
          reviews: Math.round(p.reviews * j() * scale),
          issues: Math.round(p.issues * j() * scale),
          messages: Math.round(p.messages * j() * scale),
        };
      });
    },
    async timeline(ws, f) {
      const names = new Set(swarmOf(ws).map((a) => a.name));
      return history
        .filter((e) => names.has(e.actor))
        .filter((e) => !f.kinds?.length || f.kinds.includes(e.kind))
        .filter((e) => !f.actor || e.actor === f.actor)
        .slice(0, f.limit ?? 200);
    },
    async *watchTimeline(ws, signal) {
      for await (const e of timelineHub.subscribe(signal)) if (swarmOf(ws).some((a) => a.name === e.actor)) yield e;
    },
    async throughput(ws, days) {
      const swarm = swarmOf(ws);
      const out: ThroughputDay[] = [];
      for (let i = days - 1; i >= 0; i--) {
        const t = t0 - i * DAY;
        const r = rng(hash(`tp${day(t)}`));
        const dow = new Date(t).getUTCDay();
        const weekend = dow === 0 || dow === 6 ? 0.4 : 1;
        const prs: Record<string, number> = {};
        const tokens: Record<string, number> = {};
        const spend: Record<string, number> = {};
        for (const a of swarm) {
          const builder = a.role === "implementer" ? 1 : a.role === "qa" ? 0.3 : 0.08;
          prs[a.name] = Math.round(builder * 3.2 * weekend * r() * 1.4);
          const tok = Math.round((a.role === "chronicler" ? 0.3 : 1) * weekend * (1.2e6 + r() * 2.4e6));
          tokens[a.name] = tok;
          spend[a.name] = Math.round(tok * 6e-6 * 100) / 100;
        }
        const merged = Object.values(prs).reduce((x, y) => x + y, 0);
        out.push({
          day: day(t),
          prsMerged: prs,
          reviewLatencyHours: merged ? Math.round((0.4 + r() * 2.2 + (i < 2 ? 1 : 0)) * 10) / 10 : null,
          ciFailures: Math.floor(r() * r() * 6 * weekend),
          tokens,
          spendUSD: spend,
        });
      }
      return out;
    },
    async latestReport(ws) {
      return getReports(ws)[0] ?? null;
    },
    async reports(ws, limit) {
      return getReports(ws).slice(0, limit);
    },
    async requestReport(ws) {
      await sleep(50);
      const list = getReports(ws);
      setTimeout(() => {
        const at = new Date();
        list.unshift({
          id: `R${list.length + 1}`,
          at: at.toISOString(),
          author: `iris@${ws}`,
          markdown: `## Status, ${at.toUTCString().slice(17, 22)} UTC (on request)\n\n**In flight:** hermes on idempotency keys (#147); proteus addressing review on #139.\n\n**Waiting on a human:** ${rooms().filter((x) => x.presence === "waitingOnHuman").map((x) => `**${x.name}**`).join(", ") || "nobody"}.\n\n**Risks:** review latency is still above 1.5 h.\n`,
        });
        const iris = swarm.find((a) => a.name === "iris");
        if (iris) {
          const e: ActivityEvent = { ...makeEvent(iris, Date.now(), rng(1)), kind: "message", target: "status", repo: "", summary: "sent a status report" };
          history.unshift(e);
          timelineHub.publish(e);
        }
      }, 1200);
    },
  };
}
