import { createRouterTransport } from "@connectrpc/connect";
import { describe, expect, it } from "vitest";
import * as pb from "@/gen/swarm/controlcenter/v1/controlcenter_pb";
import { createConnectApi } from "./connect";

// Unread badges, message permalinks and the live verdict stream.
function fake() {
  const calls: Record<string, unknown> = {};
  const transport = createRouterTransport(({ service }) => {
    service(pb.AuthService, { getMe: () => ({ user: { handle: "lex", role: pb.Role.OPERATOR, workspaces: ["acme", "beta", "gone"] } }) });
    service(pb.ChannelService, {
      getUnreadSummary: (r) => {
        if (r.workspace === "gone") throw new Error("no messaging for this one");
        return {
          counts:
            r.workspace === "acme"
              ? [
                  { scope: "direct", unread: 2 },
                  { scope: "mentions", unread: 1 },
                  { scope: "general", unread: 4 },
                ]
              : [{ scope: "ops", unread: 3 }],
        };
      },
      getChannelMessage: (r) => {
        calls.get = r;
        return { item: { id: r.id, channel: "general", from: "athena@acme", fromKind: "agent", body: "the whole post" } };
      },
    });
    service(pb.WardenService, {
      async *watchVerdicts(r) {
        calls.watch = r;
        yield { verdict: { id: "v1", workspace: "acme", room: "athena", severity: pb.Severity.HIGH, layer: "judge", reason: "rm -rf" } };
      },
    });
  });
  return { api: createConnectApi(transport), calls };
}

describe("connect api: unread, permalinks, verdict stream", () => {
  it("sums unread counts across workspaces, skipping ones that fail", async () => {
    const { api } = fake();
    expect(await api.unreadSummary()).toEqual({ direct: 2, mentions: 1, channels: 7 });
  });

  it("gets one message in full", async () => {
    const { api, calls } = fake();
    const m = await api.getMessage("acme", "P1");
    expect(calls.get).toMatchObject({ workspace: "acme", id: "P1" });
    expect(m).toMatchObject({ id: "P1", workspace: "acme", channel: "general", from: "athena@acme", body: "the whole post" });
  });

  it("streams verdicts", async () => {
    const { api, calls } = fake();
    const got = [];
    for await (const v of api.watchVerdicts(["acme"], new AbortController().signal)) got.push(v);
    expect(calls.watch).toMatchObject({ workspaces: ["acme"] });
    expect(got).toMatchObject([{ id: "v1", room: "athena", severity: "high", layer: "judge" }]);
  });
});
