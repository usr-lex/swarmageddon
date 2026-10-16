import type { AuthProvider } from "@/api/types";

/** Browser-redirect sign-in endpoints of the control center (§8.1). */
export function providerLoginURL(p: Pick<AuthProvider, "kind" | "name">, next = "/", test = false): string {
  const q = new URLSearchParams({ next });
  if (test) q.set("test", "1");
  return `/auth/${p.kind}/${encodeURIComponent(p.name)}/login?${q}`;
}
