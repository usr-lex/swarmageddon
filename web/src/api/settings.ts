// Layered room settings (design §6.6): env, secret env, MCP servers and
// network, set at workspace, role and room level and merged in that order.
// merge() mirrors internal/controlcenter/wizard/settings.go Merge; the server's
// result is authoritative, this one serves the mock and instant previews.

export type NetworkMode = "none" | "limited" | "full";
export const networkModes: NetworkMode[] = ["none", "limited", "full"];

export interface SecretRef {
  name: string;
  key: string;
}

/** A literal value or a Secret reference (exactly one). */
export interface McpValue {
  value: string;
  valueFrom?: SecretRef;
}

export interface McpServer {
  /** stdio or http; "" means stdio when command is set. */
  type: "" | "stdio" | "http";
  command: string;
  args: string[];
  env: Record<string, McpValue>;
  url: string;
  headers: Record<string, McpValue>;
  enabledTools: string[];
  /** Removes a server inherited from an earlier level. */
  disabled: boolean;
}

export interface NetworkSettings {
  /** "" inherits. */
  mode: NetworkMode | "";
  /** Hosts or *.suffix, optional :port; used in limited mode. */
  domains: string[];
  /** Undefined inherits. */
  packageManagers?: boolean;
}

export interface SecretEnv {
  name: string;
  secret: SecretRef;
}

/** One level of settings. */
export interface RoomSettings {
  env: Record<string, string>;
  secretEnv: SecretEnv[];
  mcpServers: Record<string, McpServer>;
  /** Undefined inherits. */
  network?: NetworkSettings;
}

export type SettingLevel = "workspace" | "role" | "room";
export const settingLevels: SettingLevel[] = ["workspace", "role", "room"];

export interface SettingSource {
  /** env, secretEnv, mcpServer, network, domain or packageManagers. */
  kind: string;
  key: string;
  /** workspace, role, room, or "default" for the built-in network mode. */
  level: SettingLevel | "default";
}

export interface EffectiveSettings {
  room: string;
  role: string;
  settings: RoomSettings;
  sources: SettingSource[];
}

/** A room's settings page: each level, the merged result and its shared volumes. */
export interface RoomSettingsView {
  workspace: RoomSettings;
  role: RoomSettings;
  room: RoomSettings;
  effective: EffectiveSettings;
  /** Shared volume name -> "ro" or "rw". */
  sharedVolumes: Record<string, "ro" | "rw">;
}

export interface SharedVolumeMount {
  /** everyone, role:<t>, group:<g> or room:<name>. */
  select: string;
  readOnly: boolean;
}

export interface SharedVolumeDraft {
  name: string;
  sizeGi: number;
  /** "" uses the RWX class preflight found. */
  storageClass: string;
  mounts: SharedVolumeMount[];
}

export interface RoomSecrets {
  /** The Secret the control center manages ("room-secrets"). */
  secretName: string;
  keys: string[];
}

export const emptySettings = (): RoomSettings => ({ env: {}, secretEnv: [], mcpServers: {} });

export const emptyMcp = (): McpServer => ({ type: "stdio", command: "", args: [], env: {}, url: "", headers: {}, enabledTools: [], disabled: false });

export function isEmptySettings(s: RoomSettings | undefined): boolean {
  return !s || (Object.keys(s.env).length === 0 && s.secretEnv.length === 0 && Object.keys(s.mcpServers).length === 0 && !s.network);
}

/** Merges levels in order; see the file comment. */
export function merge(levels: SettingLevel[], ds: (RoomSettings | undefined)[]): { settings: RoomSettings; sources: SettingSource[] } {
  const src = new Map<string, SettingSource>();
  const set = (kind: string, key: string, level: SettingLevel | "default") => src.set(`${kind}/${key}`, { kind, key, level });
  const env: Record<string, string> = {};
  const senv = new Map<string, SecretEnv>();
  const mcp: Record<string, McpServer> = {};
  const net: NetworkSettings = { mode: "full", domains: [] };
  ds.forEach((d, i) => {
    if (!d) return;
    const lvl = levels[i];
    for (const [k, v] of Object.entries(d.env)) {
      env[k] = v;
      set("env", k, lvl);
    }
    for (const e of d.secretEnv) {
      senv.set(e.name, e);
      set("secretEnv", e.name, lvl);
    }
    for (const [k, v] of Object.entries(d.mcpServers)) {
      if (v.disabled) {
        delete mcp[k];
        src.delete(`mcpServer/${k}`);
        continue;
      }
      mcp[k] = v;
      set("mcpServer", k, lvl);
    }
    const n = d.network;
    if (n) {
      if (n.mode) {
        net.mode = n.mode;
        set("network", "mode", lvl);
      }
      for (const dom of n.domains) {
        if (!net.domains.includes(dom)) {
          net.domains.push(dom);
          set("domain", dom, lvl);
        }
      }
      if (n.packageManagers !== undefined) {
        net.packageManagers = n.packageManagers;
        set("packageManagers", "", lvl);
      }
    }
  });
  if (!src.has("network/mode")) set("network", "mode", "default");
  const secretEnv = [...senv.values()].sort((a, b) => a.name.localeCompare(b.name));
  const sources = [...src.entries()].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0)).map(([, v]) => v);
  return { settings: { env, secretEnv, mcpServers: mcp, network: net }, sources };
}

/** The merged settings of one room. */
export function effectiveFor(room: string, role: string, ws?: RoomSettings, rt?: RoomSettings, own?: RoomSettings): EffectiveSettings {
  const m = merge(settingLevels, [ws, rt, own]);
  return { room, role, settings: m.settings, sources: m.sources };
}

/** Where an effective entry came from. */
export function sourceOf(e: EffectiveSettings, kind: string, key: string): SettingSource["level"] | undefined {
  return e.sources.find((s) => s.kind === kind && s.key === key)?.level;
}

const envName = /^[A-Za-z_][A-Za-z0-9_]*$/;
const domainRE = /^(\*\.)?[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*(:[0-9]{1,5})?$/;
/** Values that look like credentials don't belong in plain env (design §11.4). */
export const keyLike = /^(sk-|ghp_|gho_|github_pat_|glpat-|xox[abpr]-|AKIA)[A-Za-z0-9_-]{8,}|-----BEGIN /;
export const secretKeyPattern = /^[-._a-zA-Z0-9]+$/;
export const mountSelectorPattern = /^(everyone|(role|group|room):[a-z0-9][a-z0-9_-]{0,62})$/;

/** Problems in one level, worded like the server's ValidateSettings. */
export function validateSettings(where: string, p: RoomSettings | undefined): string[] {
  if (!p) return [];
  const out: string[] = [];
  const bad = (m: string) => out.push(`${where}: ${m}`);
  for (const [k, v] of Object.entries(p.env)) {
    if (!envName.test(k)) bad(`variable "${k}" is not a valid name`);
    if (keyLike.test(v)) bad(`variable "${k}" looks like a secret; use a secret variable instead`);
  }
  const seen = new Set<string>();
  for (const e of p.secretEnv) {
    if (!envName.test(e.name)) bad(`secret variable "${e.name}" is not a valid name`);
    if (seen.has(e.name)) bad(`secret variable "${e.name}" appears twice`);
    if (e.name in p.env) bad(`variable "${e.name}" is both plain and secret`);
    if (!e.secret.name || !e.secret.key) bad(`secret variable "${e.name}" needs a Secret name and key`);
    seen.add(e.name);
  }
  for (const [name, s] of Object.entries(p.mcpServers)) {
    if (name === "swarm") bad(`MCP server "swarm" is reserved`);
    if (s.disabled) continue;
    if ((s.type === "" || s.type === "stdio") && !s.command) bad(`MCP server "${name}" needs a command`);
    if (s.type === "http" && !s.url) bad(`MCP server "${name}" needs a URL`);
    for (const [k, v] of [...Object.entries(s.env), ...Object.entries(s.headers)]) {
      if (!v.valueFrom && keyLike.test(v.value)) bad(`MCP server "${name}": ${k} looks like a secret; take it from a Secret`);
    }
  }
  for (const d of p.network?.domains ?? []) if (!domainRE.test(d)) bad(`domain "${d}" is not a host or *.suffix`);
  return out;
}
