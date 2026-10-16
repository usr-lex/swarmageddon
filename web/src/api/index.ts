import type { Api } from "./api";

export { ApiError } from "./api";
export type { Api } from "./api";

const MOCK_KEY = "swarm-mock";

/** Which API the app talks to: "real", or a mock mode from ?mock=1 / ?mock=demo (sticky per tab). */
export function apiMode(): string {
  const param = new URLSearchParams(location.search).get("mock");
  try {
    if (param === "0") sessionStorage.removeItem(MOCK_KEY);
    else if (param) sessionStorage.setItem(MOCK_KEY, param);
    return sessionStorage.getItem(MOCK_KEY) ?? import.meta.env.VITE_MOCK ?? "real";
  } catch {
    return param ?? import.meta.env.VITE_MOCK ?? "real";
  }
}

let current: Api | undefined;

/** The API in use; set by initApi before the app renders. */
export function api(): Api {
  if (!current) throw new Error("initApi has not run");
  return current;
}

export function setApi(a: Api) {
  current = a;
}

export async function initApi(): Promise<Api> {
  const mode = apiMode();
  if (mode === "real") {
    const { createConnectApi } = await import("./connect");
    current = createConnectApi();
  } else {
    const { createMockApi } = await import("./mock");
    current = createMockApi(mode);
  }
  return current;
}
