import { FitAddon } from "@xterm/addon-fit";
import { Terminal } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import { useEffect, useRef, useState } from "react";
import { apiMode } from "@/api";
import { terminalURL, type TerminalMode } from "@/lib/sockets";
import { Badge } from "./ui";

type Status = "connecting" | "open" | "closed";

/** An xterm attached to a room through /ws/terminal. Reconnects when mode or read-only changes. */
export function TerminalView({ workspace, room, mode, readOnly }: { workspace: string; room: string; mode: TerminalMode; readOnly: boolean }) {
  const ref = useRef<HTMLDivElement>(null);
  const [status, setStatus] = useState<Status>("connecting");
  const [reason, setReason] = useState("");

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
    const ws = new WebSocket(terminalURL(workspace, room, mode, readOnly));
    ws.binaryType = "arraybuffer";
    const enc = new TextEncoder();
    const sendResize = () => {
      if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
    };
    ws.onopen = () => {
      setStatus("open");
      sendResize();
      if (!readOnly) term.focus();
    };
    ws.onmessage = (ev) => {
      if (typeof ev.data === "string") term.write(ev.data);
      else term.write(new Uint8Array(ev.data as ArrayBuffer));
    };
    ws.onclose = (ev) => {
      setStatus("closed");
      setReason(ev.reason || (ev.code === 1000 ? "Session ended." : `Disconnected (${ev.code}).`));
    };
    const data = term.onData((s) => {
      if (!readOnly && ws.readyState === WebSocket.OPEN) ws.send(enc.encode(s));
    });
    const resize = term.onResize(sendResize);
    return () => {
      data.dispose();
      resize.dispose();
      ro.disconnect();
      ws.close(1000);
      term.dispose();
    };
  }, [workspace, room, mode, readOnly]);

  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-center gap-2 text-xs" aria-live="polite">
        <Badge tone={status === "open" ? "ok" : status === "connecting" ? "info" : "bad"}>{status}</Badge>
        {readOnly && <Badge>read-only</Badge>}
        {status === "closed" && <span className="text-muted">{reason}</span>}
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
