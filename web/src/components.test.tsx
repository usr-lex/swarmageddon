import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";
import { setApi } from "@/api";
import { createMockApi, MOCK_SETUP_TOKEN } from "@/api/mock";
import { defaultRoom, emptyDraft } from "@/lib/workspace";
import { PreflightStep } from "@/pages/wizard/PreflightStep";
import { ReachDiagram } from "@/pages/workspace/ReachDiagram";

function wrap(ui: ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

describe("PreflightStep", () => {
  it("requires explicit acceptance of the privileged tier", async () => {
    sessionStorage.clear();
    const api = createMockApi("demo");
    setApi(api);
    const platform = await api.getPlatform();
    const save = vi.fn().mockResolvedValue(undefined);
    wrap(<PreflightStep platform={platform} save={save} saving={false} />);
    const user = userEvent.setup();
    const cont = screen.getByRole("button", { name: "Continue" });
    expect(cont).toBeEnabled();
    await user.click(screen.getByLabelText(/Privileged/));
    expect(screen.getByRole("alert")).toHaveTextContent("not isolated");
    expect(cont).toBeDisabled();
    await user.click(screen.getByLabelText(/I understand/));
    expect(cont).toBeEnabled();
    await user.click(cont);
    expect(save).toHaveBeenCalledWith(expect.objectContaining({ isolationTier: "privileged", privilegedAccepted: true }), "signin");
  });

  it("blocks on failed checks and shows the fix", async () => {
    sessionStorage.clear();
    const api = createMockApi("demo");
    setApi(api);
    const platform = await api.getPlatform();
    platform.preflight.checks.push({ name: "NetworkPolicy enforcement", result: "fail", message: "Connection succeeded.", fix: "Use a CNI that enforces policies." });
    wrap(<PreflightStep platform={platform} save={vi.fn()} saving={false} />);
    expect(screen.getByText("Use a CNI that enforces policies.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Continue" })).toBeDisabled();
  });
});

describe("ReachDiagram", () => {
  it("describes who can reach whom in text", () => {
    const d = { ...emptyDraft("m"), groups: ["clean", "dirty"], rooms: [{ ...defaultRoom("athena", "x", "m"), group: "clean" }, { ...defaultRoom("hecate", "x", "m"), group: "dirty" }] };
    d.links = [{ from: "dirty", to: "clean", mode: "mediated", senders: ["hecate"], recipients: ["athena"], requirePrefix: "" }];
    render(<ReachDiagram draft={d} />);
    expect(screen.getByRole("img")).toBeInTheDocument();
    expect(screen.getByText("dirty → clean: mediated from hecate to athena")).toBeInTheDocument();
    expect(screen.getByText("Agents in clean reach each other")).toBeInTheDocument();
  });
});

describe("mock API", () => {
  it("claims setup, provisions a workspace and raises an approval", async () => {
    vi.useFakeTimers();
    try {
      sessionStorage.clear();
      const api = createMockApi("1");
      await expect(api.me()).rejects.toThrow(/Sign in/);
      const admin = { username: "a", email: "a@x", password: "long enough password" };
      const p1 = expect(api.claimAdmin("nope", admin)).rejects.toThrow(/not right/);
      await vi.advanceTimersByTimeAsync(100);
      await p1;
      const p2 = expect(api.claimAdmin(MOCK_SETUP_TOKEN, { ...admin, password: "short" })).rejects.toThrow(/12/);
      await vi.advanceTimersByTimeAsync(100);
      await p2;
      const p3 = api.claimAdmin(MOCK_SETUP_TOKEN.toLowerCase(), admin);
      await vi.advanceTimersByTimeAsync(100);
      await p3;
      expect((await api.me()).role).toBe("admin");

      const draft = { ...emptyDraft("m"), name: "acme", org: "acme", rooms: [defaultRoom("athena", "implementer", "m")] };
      expect((await api.planWorkspace(draft)).changes.map((c) => `${c.op} ${c.kind}`)).toContain("create Room");
      await api.applyWorkspace(draft);
      await vi.advanceTimersByTimeAsync(2000);
      const approvals = await api.listApprovals();
      expect(approvals).toHaveLength(1);
      await api.answerApproval(approvals[0].id, "allow", "");
      expect(await api.listApprovals()).toHaveLength(0);
      const plan = await api.planWorkspace(draft);
      expect(plan.changes.every((c) => c.op === "unchanged")).toBe(true);
    } finally {
      vi.useRealTimers();
    }
  });
});
