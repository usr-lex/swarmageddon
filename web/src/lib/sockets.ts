/**
 * WebSocket endpoints of the control center (internal/controlcenter/terminal):
 *
 *  - /ws/terminal?workspace=&room=&mode=agent|view|shell: binary frames carry
 *    terminal bytes both ways; text frames carry JSON control messages,
 *    {"type":"resize","cols":N,"rows":N} from the browser and
 *    {"type":"exit","error":"..."} from the server before it closes.
 *    mode=view is the agent's tmux session read-only (tmux attach -r).
 *  - /ws/vnc?workspace=&room=: the room's VNC server, subprotocol "binary".
 */
export type TerminalMode = "agent" | "shell";

function wsBase(): string {
  return `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}`;
}

export function terminalURL(workspace: string, room: string, mode: TerminalMode, readOnly: boolean): string {
  const m = mode === "agent" && readOnly ? "view" : mode;
  return `${wsBase()}/ws/terminal?${new URLSearchParams({ workspace, room, mode: m })}`;
}

export function vncURL(workspace: string, room: string): string {
  return `${wsBase()}/ws/vnc?${new URLSearchParams({ workspace, room })}`;
}

/** A server control message, or null for plain text output. */
export function parseControl(data: string): { type: string; error?: string } | null {
  try {
    const m = JSON.parse(data);
    return m && typeof m.type === "string" ? m : null;
  } catch {
    return null;
  }
}
