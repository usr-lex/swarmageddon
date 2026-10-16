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

/** Labels for preflight check names (internal/operator/preflight). */
const checkLabels: Record<string, string> = {
  kubernetesVersion: "Kubernetes version",
  architecture: "Architecture",
  defaultStorageClass: "Default StorageClass",
  volumeSharing: "Volume sharing (ReadWriteMany)",
  volumeSizeEnforcement: "Volume size enforcement",
  idmapVolumes: "idmap support for volumes",
  networkPolicy: "NetworkPolicy enforcement",
  podmanInUserNamespace: "Podman in a user namespace",
  sysboxRuntimeClass: "Sysbox RuntimeClass",
  privilegedAdmission: "Privileged pods admitted",
  isolationTier: "Nested-container tier",
  capacity: "Free CPU, memory, disk",
  ingress: "Ingress",
  clusterDNS: "Cluster DNS",
  clusterCIDRs: "Cluster CIDRs",
  secretsEncryption: "Secrets encryption at rest",
};

export const checkLabel = (name: string) => checkLabels[name] ?? name;

/** Whether preflight found ReadWriteMany volumes, which shared volumes need. */
export const hasRWX = (p: { preflight: { checks: { name: string; result: string }[] } } | undefined) =>
  !!p?.preflight.checks.some((c) => c.name === "volumeSharing" && c.result === "pass");
