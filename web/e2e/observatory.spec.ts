import { expect, test } from "@playwright/test";

test("observatory: panels render live data and respond", async ({ page }, info) => {
  await page.goto("/?mock=demo");
  await expect(page).toHaveURL(/\/w\/acme$/);
  await expect(page.getByRole("heading", { name: /acme/ })).toBeVisible();

  // Now: waiting-on-human first.
  const rows = page.getByTestId("now-row");
  await expect(rows).toHaveCount(10);
  await expect(rows.first()).toContainText("Waiting on you");

  // Status report, and a fresh one on request.
  await expect(page.getByTestId("report")).toContainText("Shipped since the last report");
  await page.getByRole("button", { name: "Request an update now" }).click();
  await expect(page.getByTestId("report")).toContainText("on request", { timeout: 10_000 });

  // Chord: clicking a ribbon lists that pair's message metadata.
  const ribbon = page.getByRole("button", { name: /hermes and metis: \d+ messages, Mediated crossing/ });
  await ribbon.focus();
  await page.keyboard.press("Enter");
  await expect(page.getByTestId("pair-messages")).toContainText("hermes ⇄ metis");

  // Window picker refetches.
  await page.getByRole("radio", { name: "7d" }).click();
  await expect(page.getByRole("radio", { name: "7d" })).toHaveAttribute("aria-checked", "true");

  // Handoff graph, axes, heatmap, throughput.
  await expect(page.getByRole("img", { name: "Handoff graph" })).toBeVisible();
  await expect(page.getByTestId("axes-agent")).toHaveCount(10);
  await expect(page.getByRole("img", { name: "Events per day, swarm" })).toBeVisible();
  await expect(page.getByRole("img", { name: "PRs merged" })).toBeVisible();

  // Timeline filters and stays live.
  const tl = page.getByTestId("timeline");
  const before = await tl.locator("li").count();
  await page.getByRole("button", { name: "Code", exact: true }).click();
  await expect(tl.locator("li").first()).not.toContainText("message to");
  expect(before).toBeGreaterThan(10);

  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(overflow).toBeLessThanOrEqual(0);

  if (process.env.SHOTS) {
    for (const theme of ["light", "dark"] as const) {
      await page.emulateMedia({ colorScheme: theme });
      await page.waitForTimeout(1500);
      await page.screenshot({ path: `${process.env.SHOTS}/obs-${info.project.name}-${theme}.png`, fullPage: true });
    }
  }
});
