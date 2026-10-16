import { expect, test } from "@playwright/test";

// Every page fits the viewport width (no horizontal page scroll), in both themes.
const pages = [
  "/w/acme",
  "/fleet",
  "/rooms/acme/athena?tab=activity",
  "/rooms/acme/athena?tab=storage",
  "/approvals",
  "/inbox/athena@acme",
  "/workspaces",
  "/workspaces/acme/edit",
  "/spend",
  "/audit",
  "/users",
  "/settings/notifications",
];

for (const theme of ["light", "dark"] as const) {
  test(`no horizontal overflow (${theme})`, async ({ page }, info) => {
    await page.emulateMedia({ colorScheme: theme });
    await page.goto("/fleet?mock=demo");
    for (const p of pages) {
      await page.goto(p);
      await expect(page.locator("main")).toBeVisible();
      await page.waitForTimeout(300);
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
      expect(overflow, `${p} overflows by ${overflow}px`).toBeLessThanOrEqual(0);
      if (process.env.SHOTS) {
        await page.screenshot({ path: `${process.env.SHOTS}/${info.project.name}-${theme}${p.replace(/[/?=@]/g, "_")}.png`, fullPage: true });
      }
    }
  });
}
