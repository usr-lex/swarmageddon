import { render } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { createMockObservatory, demoSwarm, rng } from "@/api/mockObservatory";
import type { RoomSummary } from "@/api/types";
import { inline, renderMarkdown } from "@/lib/markdown";
import { leaning, shares } from "./Axes";
import { chordModel } from "./Chord";
import { binOf, calendar, heatBins } from "./Heatmap";

const rooms = demoSwarm.map((a) => ({ workspace: "acme", name: a.name, group: a.group, role: a.role, presence: "idle" }) as RoomSummary);

describe("chord model", () => {
  it("orders parties by group, people last, and marks crossings", async () => {
    const o = createMockObservatory(() => rooms);
    const e = await o.edges("acme", "24h");
    const m = chordModel(e);
    const ids = m.parties.map((p) => p.id);
    expect(ids.slice(-2).every((p) => p.startsWith("@"))).toBe(true);
    expect(m.parties.findIndex((p) => p.group === "dirty")).toBeGreaterThan(m.parties.findLastIndex((p) => p.group === "clean"));
    const i = ids.indexOf("hermes");
    const j = ids.indexOf("metis");
    expect(m.pairKind.get(`${Math.min(i, j)},${Math.max(i, j)}`)).toBe("mediated");
    // The wall holds: no open link from the clean side to the dirty side.
    for (const msg of e.messages) {
      const g = (n: string) => demoSwarm.find((a) => a.name === n)?.group;
      if (g(msg.from) && g(msg.to) && g(msg.from) !== g(msg.to)) expect(msg.kind).toBe("mediated");
    }
  });
});

describe("heatmap", () => {
  it("bins by quantiles with 0 as its own bin", () => {
    const bins = heatBins([0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10]);
    expect(binOf(0, bins)).toBe(0);
    expect(binOf(1, bins)).toBe(1);
    expect(binOf(10, bins)).toBe(4);
  });

  it("lays out weeks Sunday-first, ending today", () => {
    const end = Date.parse("2026-10-16T12:00:00Z"); // a Friday
    const g = calendar(new Map([["2026-10-16", 5]]), 14, end);
    const last = g.weeks[g.weeks.length - 1];
    expect(last[0].day).toBe("2026-10-11");
    expect(last[last.length - 1]).toEqual({ day: "2026-10-16", count: 5 });
    expect(g.weeks[0][0].day).toBe("2026-09-27"); // 13 days back is Sat 10-03; its week starts Sunday
  });
});

describe("activity axes", () => {
  it("computes shares and leaning", () => {
    const a = { name: "x", role: "reviewer", commits: 10, pullRequests: 0, reviews: 30, issues: 0, messages: 5 };
    expect(shares(a)).toEqual({ reviews: 75, issues: 0, pullRequests: 0, commits: 25 });
    expect(leaning(a)).toBe("reviewing");
    expect(leaning({ ...a, reviews: 0, commits: 0 })).toBe("coordinating");
    expect(leaning({ ...a, reviews: 0, commits: 0, messages: 0 })).toBe("quiet");
  });
});

describe("markdown", () => {
  it("renders a safe subset", () => {
    const { container } = render(<div>{renderMarkdown("## Title\n\nSome **bold** and `<b>code</b>`.\n\n- one\n- *two*\n")}</div>);
    expect(container.querySelector("h4")?.textContent).toBe("Title");
    expect(container.querySelector("strong")?.textContent).toBe("bold");
    expect(container.querySelector("code")?.textContent).toBe("<b>code</b>");
    expect(container.querySelector("b")).toBeNull();
    expect(container.querySelectorAll("li")).toHaveLength(2);
    expect(inline("plain")).toEqual(["plain"]);
  });
});

describe("mock generator", () => {
  it("is deterministic", () => {
    const a = rng(1);
    const b = rng(1);
    expect([a(), a(), a()]).toEqual([b(), b(), b()]);
  });
});
