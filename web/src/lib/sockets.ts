/**
 * WebSocket endpoints of the control center for terminals and the display.
 *
 * Terminal protocol (assumed until the backend lands; see the report):
 *  - binary frames carry terminal bytes both ways;
 *  - text frames carry JSON control messages from the browser:
 *    {"type":"resize","cols":N,"rows":N}.
 *  - readonly=1 makes the server drop input (tmux attach -r for mode=agent).
 */
export type TerminalMode = "agent" | "shell";

function wsBase(): string {
  return `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}`;
}

export function terminalURL(workspace: string, room: string, mode: TerminalMode, readOnly: boolean): string {
  const q = new URLSearchParams({ workspace, room, mode });
  if (readOnly) q.set("readonly", "1");
  return `${wsBase()}/ws/terminal?${q}`;
}

export function vncURL(workspace: string, room: string): string {
  return `${wsBase()}/ws/vnc?${new URLSearchParams({ workspace, room })}`;
}
