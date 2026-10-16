import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useState } from "react";
import { api } from "@/api";
import { consume } from "@/api/stream";
import type { Approval, ApprovalEvent, FleetEvent, RoomSummary } from "@/api/types";
import { sortFleet } from "./workspace";

/** Applies a fleet stream event to the current list. */
export function applyFleetEvent(rooms: RoomSummary[], ev: FleetEvent): RoomSummary[] {
  switch (ev.kind) {
    case "snapshot":
      return ev.rooms;
    case "upsert": {
      const i = rooms.findIndex((r) => r.workspace === ev.room.workspace && r.name === ev.room.name);
      if (i < 0) return [...rooms, ev.room];
      const next = [...rooms];
      next[i] = ev.room;
      return next;
    }
    case "remove":
      return rooms.filter((r) => !(r.workspace === ev.workspace && r.name === ev.name));
  }
}

/** Applies an approvals stream event to the pending list. */
export function applyApprovalEvent(list: Approval[], ev: ApprovalEvent): Approval[] {
  switch (ev.kind) {
    case "requested":
      return list.some((a) => a.id === ev.approval.id) ? list : [...list, ev.approval];
    case "answered":
    case "withdrawn":
      return list.filter((a) => a.id !== ev.id);
  }
}

/** The fleet, kept live by WatchFleet while a component uses it. */
export function useFleet() {
  const [rooms, setRooms] = useState<RoomSummary[]>();
  useEffect(() => {
    const ctl = new AbortController();
    void consume(
      (s) => api().watchFleet(s),
      (ev) => setRooms((old) => applyFleetEvent(old ?? [], ev)),
      ctl.signal,
    );
    return () => ctl.abort();
  }, []);
  return { rooms: useMemo(() => rooms && sortFleet(rooms), [rooms]) };
}

/** Pending approvals, kept live by WatchApprovals. Mounted once by the shell. */
export function useApprovalsFeed(enabled: boolean) {
  const qc = useQueryClient();
  useEffect(() => {
    if (!enabled) return;
    const ctl = new AbortController();
    void consume(
      (s) => api().watchApprovals(s),
      (ev) => {
        qc.setQueryData<Approval[]>(["approvals"], (old) => applyApprovalEvent(old ?? [], ev));
        if (ev.kind !== "requested") {
          qc.setQueryData<Approval>(["approval", ev.id], (old) =>
            old ? (ev.kind === "answered" ? { ...old, answer: ev.answer } : { ...old, withdrawn: ev.reason }) : old,
          );
        }
      },
      ctl.signal,
    );
    return () => ctl.abort();
  }, [qc, enabled]);
}

export function useApprovals() {
  return useQuery({ queryKey: ["approvals"], queryFn: () => api().listApprovals() });
}
