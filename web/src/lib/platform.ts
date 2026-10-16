import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect } from "react";
import { api } from "@/api";
import { consume } from "@/api/stream";
import type { Condition, Platform, PlatformSpec } from "@/api/types";

/** The Platform resource, kept live by WatchPlatform. */
export function useLivePlatform() {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["platform"], queryFn: () => api().getPlatform() });
  useEffect(() => {
    const ctl = new AbortController();
    void consume(
      (s) => api().watchPlatform(s),
      (p) => qc.setQueryData<Platform>(["platform"], p),
      ctl.signal,
    );
    return () => ctl.abort();
  }, [qc]);
  return q;
}

export interface ServiceProgress {
  condition: string;
  label: string;
  state: "ready" | "progress" | "waiting" | "failed";
  message: string;
}

/** Platform services in deploy order (§3.4 step 3), with their state from conditions. */
export function serviceProgress(spec: PlatformSpec, conditions: Condition[]): ServiceProgress[] {
  const services: [string, string][] = [
    ["PostgresReady", "PostgreSQL"],
    ["ForgejoReady", "Forgejo"],
    ["MessagingReady", "Messaging"],
    ["GatewayReady", "Model gateway (LiteLLM)"],
    ["RegistryCacheReady", "Registry cache"],
  ];
  if (spec.services.ntfyEnabled) services.push(["NtfyReady", "ntfy"]);
  return services.map(([type, label]) => {
    const c = conditions.find((x) => x.type === type);
    let state: ServiceProgress["state"] = "waiting";
    if (c?.status === "True") state = "ready";
    else if (c && /fail|error/i.test(c.reason)) state = "failed";
    else if (c) state = "progress";
    return { condition: type, label, state, message: c?.message || c?.reason || "" };
  });
}
