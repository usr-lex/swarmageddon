import { describe, expect, it } from "vitest";
import { effectiveFor, emptyMcp, emptySettings, merge, validateSettings, type RoomSettings } from "./settings";

const lvl = (p: Partial<RoomSettings>): RoomSettings => ({ ...emptySettings(), ...p });

describe("merge", () => {
  it("overrides by name, unions domains and removes disabled MCP servers", () => {
    const ws = lvl({ env: { A: "ws", B: "ws" }, mcpServers: { gh: { ...emptyMcp(), command: "gh" }, lin: { ...emptyMcp(), command: "lin" } }, network: { mode: "limited", domains: ["a.example"] } });
    const role = lvl({ env: { B: "role" }, mcpServers: { lin: { ...emptyMcp(), disabled: true } }, network: { mode: "", domains: ["b.example", "a.example"], packageManagers: true } });
    const room = lvl({ env: { C: "room" }, network: { mode: "none", domains: [] } });
    const e = effectiveFor("hermes", "implementer", ws, role, room);
    expect(e.settings.env).toEqual({ A: "ws", B: "role", C: "room" });
    expect(Object.keys(e.settings.mcpServers)).toEqual(["gh"]);
    expect(e.settings.network).toEqual({ mode: "none", domains: ["a.example", "b.example"], packageManagers: true });
    const src = Object.fromEntries(e.sources.map((s) => [`${s.kind}/${s.key}`, s.level]));
    expect(src).toMatchObject({ "env/A": "workspace", "env/B": "role", "env/C": "room", "network/mode": "room", "domain/b.example": "role", "packageManagers/": "role", "mcpServer/gh": "workspace" });
    expect(src["mcpServer/lin"]).toBeUndefined();
  });

  it("defaults the network to full", () => {
    const m = merge(["workspace"], [undefined]);
    expect(m.settings.network?.mode).toBe("full");
    expect(m.sources).toEqual([{ kind: "network", key: "mode", level: "default" }]);
  });
});

describe("validateSettings", () => {
  it("flags secrets in plain env, reserved and incomplete MCP servers, bad domains", () => {
    const probs = validateSettings("workspace", {
      env: { TOKEN: "ghp_abcdefghijklmnop", "bad-name": "x" },
      secretEnv: [{ name: "TOKEN", secret: { name: "room-secrets", key: "" } }],
      mcpServers: { swarm: { ...emptyMcp(), command: "x" }, web: { ...emptyMcp(), type: "http" } },
      network: { mode: "limited", domains: ["ok.example", "not a domain"] },
    });
    expect(probs.join("\n")).toMatch(/TOKEN" looks like a secret/);
    expect(probs.join("\n")).toMatch(/"bad-name" is not a valid name/);
    expect(probs.join("\n")).toMatch(/both plain and secret/);
    expect(probs.join("\n")).toMatch(/needs a Secret name and key/);
    expect(probs.join("\n")).toMatch(/"swarm" is reserved/);
    expect(probs.join("\n")).toMatch(/"web" needs a URL/);
    expect(probs.join("\n")).toMatch(/"not a domain"/);
  });
});
