import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useId, useState } from "react";
import { api } from "@/api";
import {
  emptyMcp,
  keyLike,
  networkModes,
  type EffectiveSettings,
  type McpServer,
  type McpValue,
  type NetworkMode,
  type NetworkSettings,
  type RoomSettings,
  type SettingSource,
} from "@/api/settings";
import { Badge, Button, ErrorText, Input, Select, cx } from "./ui";

const ROOM_SECRETS = "room-secrets";

/** Rows of a string map, kept locally so a half-typed key doesn't vanish. */
function useRows<T>(init: Record<string, T>, onChange: (m: Record<string, T>) => void) {
  const [rows, setRows] = useState<[string, T][]>(() => Object.entries(init));
  const commit = (next: [string, T][]) => {
    setRows(next);
    onChange(Object.fromEntries(next.filter(([k]) => k)));
  };
  return [rows, commit] as const;
}

function KeyValues({ label, value, onChange, disabled }: { label: string; value: Record<string, string>; onChange: (m: Record<string, string>) => void; disabled?: boolean }) {
  const [rows, commit] = useRows(value, onChange);
  return (
    <fieldset className="space-y-2">
      <legend className="label mb-1">{label}</legend>
      {rows.map(([k, v], i) => (
        <div key={i} className="flex flex-wrap gap-2">
          <Input aria-label={`${label} name`} className="!w-40 font-mono" value={k} placeholder="NAME" disabled={disabled} onChange={(e) => commit(rows.map((r, j) => (j === i ? [e.target.value, r[1]] : r)))} />
          <Input aria-label={`${label} value`} className="!w-auto flex-1 font-mono" value={v} disabled={disabled} onChange={(e) => commit(rows.map((r, j) => (j === i ? [r[0], e.target.value] : r)))} />
          {!disabled && (
            <Button variant="ghost" aria-label={`Remove ${k || "row"}`} onClick={() => commit(rows.filter((_, j) => j !== i))}>
              ×
            </Button>
          )}
          {keyLike.test(v) && <p className="w-full text-xs text-alert">This looks like a credential; store it as a secret variable instead.</p>}
        </div>
      ))}
      {!disabled && (
        <Button variant="ghost" onClick={() => commit([...rows, ["", ""]])}>
          + Add
        </Button>
      )}
    </fieldset>
  );
}

/** Keys of room-secrets, and a form to store a new value (write-only). */
function SecretKeys({ workspace, canStore }: { workspace: string; canStore: boolean }) {
  const qc = useQueryClient();
  const keys = useQuery({ queryKey: ["roomSecrets", workspace], queryFn: () => api().listRoomSecrets(workspace), enabled: canStore });
  const [key, setKey] = useState("");
  const [value, setValue] = useState("");
  const store = useMutation({
    mutationFn: () => api().setRoomSecret(workspace, key.trim(), value),
    onSuccess: async () => {
      setKey("");
      setValue("");
      await qc.invalidateQueries({ queryKey: ["roomSecrets", workspace] });
    },
  });
  const listId = `secret-keys-${workspace}`;
  return (
    <div className="space-y-2 border border-dashed border-border p-2">
      <datalist id={listId}>{keys.data?.keys.map((k) => <option key={k} value={k} />)}</datalist>
      {canStore ? (
        <>
          <p className="text-xs text-muted">
            Values go into the Secret <code>{ROOM_SECRETS}</code> of the workspace and never come back. Stored keys:{" "}
            {keys.data?.keys.length ? keys.data.keys.map((k) => <code key={k} className="mr-1">{k}</code>) : "none yet"}.
          </p>
          <div className="flex flex-wrap gap-2">
            <Input aria-label="Secret key" className="!w-40 font-mono" placeholder="key" value={key} onChange={(e) => setKey(e.target.value)} />
            <Input aria-label="Secret value" type="password" autoComplete="off" className="!w-auto flex-1" placeholder="value" value={value} onChange={(e) => setValue(e.target.value)} />
            <Button disabled={!key.trim() || !value} busy={store.isPending} onClick={() => store.mutate()}>
              Store secret
            </Button>
          </div>
          <ErrorText error={store.error} />
        </>
      ) : (
        <p className="text-xs text-muted">Store secret values once the workspace exists; reference their keys in {ROOM_SECRETS} here meanwhile.</p>
      )}
    </div>
  );
}

function SecretEnvRows({ value, onChange, workspace, disabled }: { value: RoomSettings["secretEnv"]; onChange: (v: RoomSettings["secretEnv"]) => void; workspace: string; disabled?: boolean }) {
  const listId = `secret-keys-${workspace}`;
  return (
    <fieldset className="space-y-2">
      <legend className="label mb-1">Secret environment</legend>
      {value.map((e, i) => (
        <div key={i} className="flex flex-wrap items-center gap-2">
          <Input aria-label="Secret variable name" className="!w-40 font-mono" value={e.name} placeholder="NAME" disabled={disabled} onChange={(x) => onChange(value.map((r, j) => (j === i ? { ...r, name: x.target.value } : r)))} />
          <span className="text-xs text-muted">from</span>
          <Input aria-label="Secret name" className="!w-36 font-mono" value={e.secret.name} disabled={disabled} onChange={(x) => onChange(value.map((r, j) => (j === i ? { ...r, secret: { ...r.secret, name: x.target.value } } : r)))} />
          <Input aria-label="Secret key" list={listId} className="!w-36 font-mono" value={e.secret.key} placeholder="key" disabled={disabled} onChange={(x) => onChange(value.map((r, j) => (j === i ? { ...r, secret: { ...r.secret, key: x.target.value } } : r)))} />
          {!disabled && (
            <Button variant="ghost" aria-label={`Remove ${e.name || "secret variable"}`} onClick={() => onChange(value.filter((_, j) => j !== i))}>
              ×
            </Button>
          )}
        </div>
      ))}
      {!disabled && (
        <Button variant="ghost" onClick={() => onChange([...value, { name: "", secret: { name: ROOM_SECRETS, key: "" } }])}>
          + Add secret variable
        </Button>
      )}
    </fieldset>
  );
}

function McpValues({ label, value, onChange, workspace }: { label: string; value: Record<string, McpValue>; onChange: (m: Record<string, McpValue>) => void; workspace: string }) {
  const [rows, commit] = useRows(value, onChange);
  const listId = `secret-keys-${workspace}`;
  return (
    <fieldset className="space-y-1">
      <legend className="label">{label}</legend>
      {rows.map(([k, v], i) => (
        <div key={i} className="flex flex-wrap gap-2">
          <Input aria-label={`${label} name`} className="!w-36 font-mono" value={k} onChange={(e) => commit(rows.map((r, j) => (j === i ? [e.target.value, r[1]] : r)))} />
          <Select
            aria-label={`${label} source`}
            className="!w-28"
            value={v.valueFrom ? "secret" : "value"}
            onChange={(e) => commit(rows.map((r, j) => (j === i ? [r[0], e.target.value === "secret" ? { value: "", valueFrom: { name: ROOM_SECRETS, key: "" } } : { value: "" }] : r)))}
          >
            <option value="value">value</option>
            <option value="secret">secret</option>
          </Select>
          {v.valueFrom ? (
            <Input aria-label={`${label} secret key`} list={listId} className="!w-auto flex-1 font-mono" placeholder="key in room-secrets" value={v.valueFrom.key} onChange={(e) => commit(rows.map((r, j) => (j === i ? [r[0], { value: "", valueFrom: { name: v.valueFrom!.name, key: e.target.value } }] : r)))} />
          ) : (
            <Input aria-label={`${label} value`} className="!w-auto flex-1 font-mono" value={v.value} onChange={(e) => commit(rows.map((r, j) => (j === i ? [r[0], { value: e.target.value }] : r)))} />
          )}
          <Button variant="ghost" aria-label="Remove" onClick={() => commit(rows.filter((_, j) => j !== i))}>
            ×
          </Button>
        </div>
      ))}
      <Button variant="ghost" onClick={() => commit([...rows, ["", { value: "" }]])}>
        + Add
      </Button>
    </fieldset>
  );
}

function McpServerCard({ name, server, onChange, onRename, onRemove, workspace }: { name: string; server: McpServer; onChange: (s: McpServer) => void; onRename: (n: string) => void; onRemove: () => void; workspace: string }) {
  const [draftName, setDraftName] = useState(name);
  return (
    <div className="space-y-2 border border-border p-2" data-testid={`mcp-${name}`}>
      <div className="flex flex-wrap items-center gap-2">
        <Input aria-label="MCP server name" className="!w-40 font-mono" value={draftName} onChange={(e) => setDraftName(e.target.value)} onBlur={() => draftName && draftName !== name && onRename(draftName)} />
        <label className="flex items-center gap-1 text-sm">
          <input type="checkbox" className="accent-accent" checked={server.disabled} onChange={(e) => onChange({ ...server, disabled: e.target.checked })} />
          Remove the inherited server
        </label>
        <Button variant="ghost" className="ml-auto" onClick={onRemove}>
          Delete
        </Button>
      </div>
      {!server.disabled && (
        <>
          <div className="flex flex-wrap gap-2">
            <Select aria-label="Transport" className="!w-28" value={server.type || "stdio"} onChange={(e) => onChange({ ...server, type: e.target.value as McpServer["type"] })}>
              <option value="stdio">stdio</option>
              <option value="http">http</option>
            </Select>
            {server.type === "http" ? (
              <Input aria-label="URL" className="!w-auto flex-1 font-mono" placeholder="https://…" value={server.url} onChange={(e) => onChange({ ...server, url: e.target.value })} />
            ) : (
              <>
                <Input aria-label="Command" className="!w-40 font-mono" placeholder="npx" value={server.command} onChange={(e) => onChange({ ...server, command: e.target.value })} />
                <Input aria-label="Arguments" className="!w-auto flex-1 font-mono" placeholder="-y @scope/server" value={server.args.join(" ")} onChange={(e) => onChange({ ...server, args: e.target.value.split(/\s+/).filter(Boolean) })} />
              </>
            )}
          </div>
          {server.type === "http" ? (
            <McpValues label="Headers" value={server.headers} onChange={(headers) => onChange({ ...server, headers })} workspace={workspace} />
          ) : (
            <McpValues label="Environment" value={server.env} onChange={(env) => onChange({ ...server, env })} workspace={workspace} />
          )}
          <Input
            aria-label="Enabled tools"
            placeholder="Enabled tools, comma separated (empty: all)"
            value={server.enabledTools.join(", ")}
            onChange={(e) => onChange({ ...server, enabledTools: e.target.value.split(",").map((t) => t.trim()).filter(Boolean) })}
          />
        </>
      )}
    </div>
  );
}

const modeHint: Record<NetworkMode, string> = {
  none: "No outbound connections except the platform (gateway, forge, messaging).",
  limited: "Only the domains below, plus package managers if allowed.",
  full: "Any public address; cluster and private ranges stay blocked.",
};

function NetworkEditor({ value, onChange, inherited }: { value?: NetworkSettings; onChange: (n?: NetworkSettings) => void; inherited?: boolean }) {
  const id = useId();
  const n = value ?? { mode: "", domains: [] };
  const set = (p: Partial<NetworkSettings>) => onChange({ ...n, ...p });
  const [domains, setDomains] = useState(n.domains.join("\n"));
  return (
    <fieldset className="space-y-2">
      <legend className="label mb-1">Network</legend>
      <div className="flex flex-wrap items-center gap-2">
        <label htmlFor={`${id}-mode`} className="text-sm">
          Mode
        </label>
        <Select id={`${id}-mode`} className="!w-40" value={value ? n.mode : "inherit"} onChange={(e) => (e.target.value === "inherit" ? onChange(undefined) : set({ mode: e.target.value as NetworkMode }))}>
          <option value="inherit">{inherited ? "inherit" : "default (full)"}</option>
          {networkModes.map((m) => (
            <option key={m} value={m}>
              {m}
            </option>
          ))}
        </Select>
        {n.mode && <span className="text-xs text-muted">{modeHint[n.mode]}</span>}
      </div>
      {value && (
        <>
          <label className="flex flex-col gap-1 text-sm">
            <span>Allowed domains, one per line (hosts or *.suffix, optional :port); levels add up</span>
            <textarea
              className="min-h-20 rounded-xs border border-border bg-surface p-2 font-mono text-sm"
              value={domains}
              onChange={(e) => {
                setDomains(e.target.value);
                set({ domains: e.target.value.split(/\s+/).filter(Boolean) });
              }}
            />
          </label>
          <label className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              className="accent-accent"
              checked={!!n.packageManagers}
              ref={(el) => {
                if (el) el.indeterminate = n.packageManagers === undefined;
              }}
              onChange={(e) => set({ packageManagers: e.target.checked })}
            />
            Package managers (npm, PyPI, Go proxy, crates.io, …){n.packageManagers === undefined && <span className="text-xs text-muted">(inherited)</span>}
          </label>
        </>
      )}
    </fieldset>
  );
}

/**
 * Edits one level of layered room settings (§6.6). `workspaceExists` enables
 * storing secret values with SetRoomSecret.
 */
export function SettingsEditor({
  value,
  onChange,
  workspace,
  workspaceExists,
  level,
}: {
  value: RoomSettings;
  onChange: (s: RoomSettings) => void;
  workspace: string;
  workspaceExists: boolean;
  level: "workspace" | "role" | "room";
}) {
  const names = Object.keys(value.mcpServers);
  const rename = (from: string, to: string) =>
    onChange({ ...value, mcpServers: Object.fromEntries(Object.entries(value.mcpServers).map(([k, v]) => [k === from ? to : k, v])) });
  return (
    <div className="space-y-4">
      <KeyValues label="Environment" value={value.env} onChange={(env) => onChange({ ...value, env })} />
      <SecretEnvRows value={value.secretEnv} onChange={(secretEnv) => onChange({ ...value, secretEnv })} workspace={workspace} />
      <SecretKeys workspace={workspace} canStore={workspaceExists} />
      <fieldset className="space-y-2">
        <legend className="label mb-1">MCP servers</legend>
        {names.map((n) => (
          <McpServerCard
            key={n}
            name={n}
            server={value.mcpServers[n]}
            workspace={workspace}
            onChange={(s) => onChange({ ...value, mcpServers: { ...value.mcpServers, [n]: s } })}
            onRename={(to) => rename(n, to)}
            onRemove={() => onChange({ ...value, mcpServers: Object.fromEntries(Object.entries(value.mcpServers).filter(([k]) => k !== n)) })}
          />
        ))}
        <Button
          variant="ghost"
          onClick={() => {
            let i = 1;
            while (`server-${i}` in value.mcpServers) i++;
            onChange({ ...value, mcpServers: { ...value.mcpServers, [`server-${i}`]: emptyMcp() } });
          }}
        >
          + Add MCP server
        </Button>
      </fieldset>
      <NetworkEditor value={value.network} onChange={(network) => onChange({ ...value, network })} inherited={level !== "workspace"} />
    </div>
  );
}

const levelTone: Record<SettingSource["level"], "info" | "muted" | "ok" | "warn"> = { workspace: "muted", role: "info", room: "ok", default: "muted" };

function From({ e, kind, k }: { e: EffectiveSettings; kind: string; k: string }) {
  const lvl = e.sources.find((s) => s.kind === kind && s.key === k)?.level;
  return lvl ? (
    <Badge tone={levelTone[lvl]} className="ml-2 font-mono text-[10px]">
      {lvl}
    </Badge>
  ) : null;
}

/** The merged settings of one room, each entry labelled with the level it came from. */
export function EffectiveView({ effective, className }: { effective: EffectiveSettings; className?: string }) {
  const s = effective.settings;
  const env = Object.entries(s.env);
  const mcp = Object.entries(s.mcpServers);
  const net = s.network;
  return (
    <div className={cx("space-y-3 text-sm", className)} data-testid="effective-settings">
      <p className="label">
        Effective for {effective.room}
        {effective.role && ` (${effective.role})`}
      </p>
      <section>
        <h4 className="label">Network</h4>
        <p className="font-mono">
          {net?.mode ?? "full"}
          <From e={effective} kind="network" k="mode" />
        </p>
        {!!net?.domains.length && (
          <ul className="mt-1 space-y-0.5 font-mono text-xs">
            {net.domains.map((d) => (
              <li key={d}>
                {d}
                <From e={effective} kind="domain" k={d} />
              </li>
            ))}
          </ul>
        )}
        {net?.packageManagers !== undefined && (
          <p className="text-xs">
            package managers {net.packageManagers ? "allowed" : "blocked"}
            <From e={effective} kind="packageManagers" k="" />
          </p>
        )}
      </section>
      <section>
        <h4 className="label">Environment</h4>
        {env.length + s.secretEnv.length === 0 && <p className="text-xs text-muted">none</p>}
        <ul className="space-y-0.5 font-mono text-xs">
          {env.map(([k, v]) => (
            <li key={k} className="break-all">
              {k}={v}
              <From e={effective} kind="env" k={k} />
            </li>
          ))}
          {s.secretEnv.map((e) => (
            <li key={e.name}>
              {e.name} ← {e.secret.name}/{e.secret.key}
              <From e={effective} kind="secretEnv" k={e.name} />
            </li>
          ))}
        </ul>
      </section>
      <section>
        <h4 className="label">MCP servers</h4>
        {mcp.length === 0 && <p className="text-xs text-muted">only swarm</p>}
        <ul className="space-y-0.5 font-mono text-xs">
          {mcp.map(([k, v]) => (
            <li key={k} className="break-all">
              {k}: {v.type === "http" ? v.url : [v.command, ...v.args].join(" ")}
              <From e={effective} kind="mcpServer" k={k} />
            </li>
          ))}
        </ul>
      </section>
    </div>
  );
}
