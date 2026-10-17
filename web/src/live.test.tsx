import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { setApi } from "@/api";
import { createMockApi } from "@/api/mock";
import { WardenPanel } from "@/components/Warden";

describe("WardenPanel", () => {
  it("shows verdicts decided elsewhere as they happen", async () => {
    sessionStorage.clear();
    const api = createMockApi("demo");
    setApi(api);
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <WardenPanel workspace="acme" room="hephaestus" canOperate />
      </QueryClientProvider>,
    );
    expect((await screen.findAllByTestId("verdict")).length).toBeGreaterThan(0);
    expect(screen.queryByText(/dismissed by/)).toBeNull();
    const [v] = await api.listVerdicts("acme", "hephaestus");
    // Someone else dismisses it; the stream brings the change in.
    await api.decideVerdict(v.id, "dismissed");
    expect(await screen.findByText(/dismissed by admin/)).toBeInTheDocument();
  });
});

describe("mock unread summary and permalinks", () => {
  it("counts unread items and gets one message", async () => {
    sessionStorage.clear();
    const api = createMockApi("demo");
    const sum = await api.unreadSummary();
    const page = await api.readMessages("acme", "all", { unreadOnly: true, limit: 50 });
    expect(sum.direct + sum.channels).toBe(page.items.length);
    const first = page.items[0];
    expect(await api.getMessage("acme", first.id)).toMatchObject({ id: first.id, body: first.body });
    await expect(api.getMessage("acme", "nope")).rejects.toThrow(/No such message/);
  });
});
