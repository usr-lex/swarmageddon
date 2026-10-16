import { ApiError, type Api } from "./api";

/**
 * The real API over the control center's Connect services. Placeholder until
 * proto/swarm/controlcenter/v1 lands; every call fails clearly.
 */
export function createConnectApi(): Api {
  const fail = () => {
    throw new ApiError("unknown", "The control center API is not wired yet.");
  };
  return new Proxy({} as Api, { get: () => fail });
}
