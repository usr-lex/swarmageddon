import { useEffect, useRef, useState } from "react";
import { apiMode } from "@/api";
import { vncURL } from "@/lib/sockets";
import { Badge, Button } from "./ui";

type RFBType = {
  scaleViewport: boolean;
  resizeSession: boolean;
  viewOnly: boolean;
  disconnect(): void;
  addEventListener(ev: string, fn: (e: CustomEvent) => void): void;
};

/** The room's display through noVNC (§8.4). The control center proxies /ws/vnc to the room's x11vnc. */
export function DisplayView({ workspace, room }: { workspace: string; room: string }) {
  const ref = useRef<HTMLDivElement>(null);
  const rfbRef = useRef<RFBType | null>(null);
  const [status, setStatus] = useState("connecting");
  const [viewOnly, setViewOnly] = useState(true);
  const mock = apiMode() !== "real";

  useEffect(() => {
    if (mock) return;
    let rfb: RFBType | undefined;
    let cancelled = false;
    void import("@novnc/novnc").then(({ default: RFB }) => {
      if (cancelled || !ref.current) return;
      rfb = new RFB(ref.current, vncURL(workspace, room), { wsProtocols: ["binary"] }) as RFBType;
      rfb.scaleViewport = true;
      rfb.resizeSession = false;
      rfb.viewOnly = true;
      rfb.addEventListener("connect", () => setStatus("connected"));
      rfb.addEventListener("disconnect", (e) => setStatus(e.detail?.clean ? "disconnected" : "connection lost"));
      // Rooms serve the display without a password; one that asks runs an
      // older room image. Say so instead of prompting for a password nobody has.
      rfb.addEventListener("credentialsrequired", () => {
        setStatus("display asks for a password: recreate the room to update its image");
        rfb?.disconnect();
      });
      rfbRef.current = rfb;
    });
    return () => {
      cancelled = true;
      rfb?.disconnect();
      rfbRef.current = null;
    };
  }, [workspace, room, mock]);

  useEffect(() => {
    if (rfbRef.current) rfbRef.current.viewOnly = viewOnly;
  }, [viewOnly]);

  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-2 text-xs">
        <Badge tone={status === "connected" ? "ok" : status.startsWith("display asks") ? "bad" : "info"}>{mock ? "mock: no display" : status}</Badge>
        <Button onClick={() => setViewOnly(!viewOnly)} aria-pressed={!viewOnly}>
          {viewOnly ? "Take control" : "View only"}
        </Button>
      </div>
      <div ref={ref} data-testid="display" aria-label={`Display of ${room}`} className="h-[60vh] min-h-64 overflow-hidden rounded-xs border border-border bg-black" />
    </div>
  );
}
