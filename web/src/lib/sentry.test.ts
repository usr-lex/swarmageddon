import { afterEach, describe, expect, it, vi } from "vitest";

const handler = vi.fn();
const sdk = vi.hoisted(() => ({
  init: vi.fn(),
  tanstackRouterBrowserTracingIntegration: vi.fn(() => ({ name: "TanstackRouterBrowserTracing" })),
  reactErrorHandler: vi.fn(),
}));
vi.mock("@sentry/react", () => sdk);

import { fetchSentryConfig, initSentry, reportCaughtError } from "./sentry";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.clearAllMocks();
});

describe("initSentry", () => {
  it("loads nothing without a DSN", async () => {
    expect(await initSentry({}, async () => undefined)).toBe(false);
    expect(await initSentry({}, async () => ({ dsn: "", release: "r", tracesSampleRate: 1 }))).toBe(false);
    expect(sdk.init).not.toHaveBeenCalled();
  });

  it("starts errors and route tracing, never replay", async () => {
    sdk.reactErrorHandler.mockReturnValue(handler);
    const router = { id: "router" };
    const ok = await initSentry(router, async () => ({
      dsn: "https://pub@o1.ingest.example/2",
      environment: "prod",
      release: "swarmageddon@0.1.0",
      tracesSampleRate: 0.1,
    }));
    expect(ok).toBe(true);
    expect(sdk.tanstackRouterBrowserTracingIntegration).toHaveBeenCalledWith(router);
    const opts = sdk.init.mock.calls[0][0];
    expect(opts).toMatchObject({
      dsn: "https://pub@o1.ingest.example/2",
      environment: "prod",
      release: "swarmageddon@0.1.0",
      tracesSampleRate: 0.1,
      dataCollection: { userInfo: false, cookies: false, httpBodies: [] },
    });
    expect(opts.integrations.map((i: { name: string }) => i.name)).toEqual(["TanstackRouterBrowserTracing"]);
    expect(opts).not.toHaveProperty("replaysSessionSampleRate");

    const err = new Error("render failed");
    const spy = vi.spyOn(console, "error").mockImplementation(() => {});
    reportCaughtError(err, { componentStack: "at Room" });
    expect(handler).toHaveBeenCalledWith(err, { componentStack: "at Room" });
    expect(spy).toHaveBeenCalledWith(err);
  });
});

describe("fetchSentryConfig", () => {
  it("reads the control center's client config", async () => {
    const cfg = { dsn: "https://pub@o1.ingest.example/2", release: "r", tracesSampleRate: 0 };
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({ sentry: cfg }))));
    expect(await fetchSentryConfig()).toEqual(cfg);
    vi.stubGlobal("fetch", vi.fn(async () => new Response("{}")));
    expect(await fetchSentryConfig()).toBeUndefined();
    vi.stubGlobal("fetch", vi.fn(async () => new Response("not found", { status: 404 })));
    expect(await fetchSentryConfig()).toBeUndefined();
    vi.stubGlobal("fetch", vi.fn(async () => Promise.reject(new TypeError("offline"))));
    expect(await fetchSentryConfig()).toBeUndefined();
  });
});
