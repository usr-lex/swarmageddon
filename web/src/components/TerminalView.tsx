import { FitAddon } from "@xterm/addon-fit";
import { Terminal } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import { useEffect, useRef, useState } from "react";
import { apiMode } from "@/api";
import { parseControl, terminalURL, type TerminalMode } from "@/lib/sockets";
import { Badge } from "./ui";

type Status = "connecting" | "open" | "closed";

/** An xterm attached to a room through /ws/terminal. Reconnects when mode or read-only changes, and the agent session whenever it closes. */
export function TerminalView({ workspace, room, mode, readOnly }: { workspace: string; room: string; mode: TerminalMode; readOnly: boolean }) {
  const ref = useRef<HTMLDivElement>(null);
  const [status, setStatus] = useState<Status>("connecting");
  const [reason, setReason] = useState("");
  const [recording, setRecording] = useState(false);

  useEffect(() => {
    const el = ref.current!;
    const dark = document.documentElement.classList.contains("dark");
    const term = new Terminal({
      cursorBlink: !readOnly,
      disableStdin: readOnly,
      fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace",
      fontSize: 13,
      scrollback: 5000,
      theme: dark ? { background: "#0b0d12", foreground: "#e8eaee" } : { background: "#ffffff", foreground: "#16181d", cursor: "#16181d", selectionBackground: "#c5d0fa" },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(el);
    fit.fit();
    const ro = new ResizeObserver(() => fit.fit());
    ro.observe(el);

    if (apiMode() !== "real") {
      // Mock mode: a local echo so the page is usable without a backend.
      setStatus("open");
      term.writeln(`\x1b[2m[mock] ${mode} terminal for ${room}@${workspace}${readOnly ? " (read-only)" : ""}\x1b[0m`);
      term.write(`${room}@${workspace}:~$ `);
      const d = term.onData((s) => {
        if (readOnly) return;
        term.write(s === "\r" ? `\r\n${room}@${workspace}:~$ ` : s);
      });
      return () => {
        d.dispose();
        ro.disconnect();
        term.dispose();
      };
    }

    setStatus("connecting");
    setRecording(false);
    const enc = new TextEncoder();
    let ws: WebSocket | undefined;
    let disposed = false;
    let retry: ReturnType<typeof setTimeout> | undefined;
    let backoff = 1000;
    let failures = 0;
    const sendResize = () => {
      if (ws?.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
    };
    // The agent session reconnects: its TUI restarts in place (Ctrl+C quits
    // it) and, while a room starts, the session doesn't exist yet.
    const connect = () => {
      const sock = new WebSocket(terminalURL(workspace, room, mode, readOnly));
      ws = sock;
      sock.binaryType = "arraybuffer";
      sock.onopen = () => {
        setStatus("open");
        setReason("");
        sendResize();
        if (!readOnly) term.focus();
      };
      let exitError = "";
      let opened = false;
      sock.onmessage = (ev) => {
        opened = true;
        if (typeof ev.data === "string") {
          const c = parseControl(ev.data);
          if (c?.type === "exit") exitError = c.error ?? "";
          else if (c?.type === "recording") setRecording(true);
          else if (!c) term.write(ev.data);
        } else term.write(new Uint8Array(ev.data as ArrayBuffer));
      };
      sock.onclose = (ev) => {
        if (disposed) return;
        const why = exitError || ev.reason || (ev.code === 1000 ? "Session ended." : `Disconnected (${ev.code}).`);
        failures = opened ? 0 : failures + 1;
        // Refusals (sign-in, role) also arrive as a close: stop after about a minute.
        if (mode !== "agent" || failures > 6) {
          setStatus("closed");
          setReason(why);
          return;
        }
        if (opened) backoff = 1000;
        setStatus("connecting");
        setReason(opened ? "The agent's session ended; reattaching." : `The agent's session isn't up yet (${why}); retrying.`);
        retry = setTimeout(connect, backoff);
        backoff = Math.min(backoff * 2, 10000);
      };
    };
    connect();
    const data = term.onData((s) => {
      if (!readOnly && ws?.readyState === WebSocket.OPEN) ws.send(enc.encode(s));
    });
    const resize = term.onResize(sendResize);
    return () => {
      disposed = true;
      clearTimeout(retry);
      data.dispose();
      resize.dispose();
      ro.disconnect();
      ws?.close(1000);
      term.dispose();
    };
  }, [workspace, room, mode, readOnly]);

  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-center gap-2 text-xs" aria-live="polite">
        <Badge tone={status === "open" ? "ok" : status === "connecting" ? "info" : "bad"}>{status}</Badge>
        {readOnly && <Badge>read-only</Badge>}
        {recording && <Badge tone="warn">input recorded</Badge>}
        {reason && status !== "open" && <span className="text-muted">{reason}</span>}
        {mode === "agent" && !readOnly && status === "open" && <span className="text-muted">Ctrl+C quits the agent's TUI; it restarts in a second.</span>}
      </div>
      <div
        ref={ref}
        data-testid={`terminal-${mode}`}
        role="application"
        aria-label={`${mode === "agent" ? "Agent session" : "Shell"} terminal for ${room}${readOnly ? ", read-only" : ""}`}
        className="h-[60vh] min-h-64 overflow-hidden rounded-xs border border-border bg-white dark:bg-[#0b0d12]"
      />
    </div>
  );
}
