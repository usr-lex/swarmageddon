import type { ErrorInfo } from "react";

/** The web UI's Sentry settings, from GET /client-config.json (internal/telemetry BrowserConfig). */
export interface SentryConfig {
  dsn: string;
  environment?: string;
  release: string;
  tracesSampleRate: number;
}

/** Reads /client-config.json; undefined when the control center doesn't report errors, or can't be asked. */
export async function fetchSentryConfig(): Promise<SentryConfig | undefined> {
  try {
    const res = await fetch("/client-config.json", { credentials: "same-origin" });
    if (!res.ok) return undefined;
    return ((await res.json()) as { sentry?: SentryConfig }).sentry;
  } catch {
    return undefined;
  }
}

let reactHandler: ((error: unknown, info: ErrorInfo) => void) | undefined;

/**
 * Starts Sentry when the control center has a DSN configured. Only then is the
 * SDK loaded, as its own chunk. Errors and performance traces only: no session
 * replay, which could record secrets shown on screen.
 */
export async function initSentry(router: unknown, config = fetchSentryConfig): Promise<boolean> {
  try {
    const c = await config();
    if (!c?.dsn) return false;
    const Sentry = await import("./sentry-sdk");
    Sentry.init({
      dsn: c.dsn,
      environment: c.environment || undefined,
      release: c.release,
      tracesSampleRate: c.tracesSampleRate,
      // Page loads and navigations named by route (/rooms/$workspace/$room),
      // not by the names in the URL.
      integrations: [Sentry.tanstackRouterBrowserTracingIntegration(router)],
      // No user, cookies or bodies (what people type into the API).
      dataCollection: { userInfo: false, cookies: false, httpBodies: [] },
    });
    reactHandler = Sentry.reactErrorHandler();
    return true;
  } catch {
    return false;
  }
}

/**
 * createRoot's onCaughtError: errors an error boundary caught, which Sentry's
 * global handlers don't see. Logged as React does by default, and reported
 * once Sentry is running.
 */
export function reportCaughtError(error: unknown, info: ErrorInfo): void {
  console.error(error);
  reactHandler?.(error, info);
}
