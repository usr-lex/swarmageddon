import type { AuthProvider } from "@/api/types";

/**
 * Browser-redirect sign-in (§8.1): GET /auth/{oidc|saml}/{name}/login?return=/path.
 * The server's ListProviders gives loginURL; the fallback builds the same path.
 */
export function providerLoginURL(p: Pick<AuthProvider, "kind" | "name"> & { loginURL?: string }, returnTo = "/"): string {
  const base = p.loginURL || `/auth/${p.kind}/${encodeURIComponent(p.name)}/login`;
  return `${base}${base.includes("?") ? "&" : "?"}${new URLSearchParams({ return: returnTo })}`;
}
