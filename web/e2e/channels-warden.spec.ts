import { expect, test } from "@playwright/test";

test("channels: unread, mention autocomplete, post, delivery", async ({ page }) => {
  await page.goto("/channels?mock=demo");
  await expect(page.getByRole("heading", { name: "Unread" })).toBeVisible();
  // Mentions of me are highlighted.
  await expect(page.locator('[data-mentions-me="true"]').first()).toContainText("@admin");

  // Open a channel from the sidebar (on a phone the sidebar shows on the unread view).
  await page.getByTestId("channels-acme").getByRole("link", { name: /general-clean/ }).click();
  await expect(page).toHaveURL(/\/channels\/acme\/general-clean$/);
  await expect(page.getByRole("log", { name: "Posts" })).toContainText("Checkout e2e on staging");

  // @mention autocomplete.
  const box = page.getByRole("combobox", { name: "Post to #general-clean" });
  await box.fill("Thanks @her");
  await expect(page.getByRole("listbox", { name: "Mention" })).toContainText("@hermes");
  await box.press("Enter");
  await expect(box).toHaveValue("Thanks @hermes ");
  await box.pressSequentially("please rebase");
  await page.getByRole("button", { name: "Post", exact: true }).click();
  await expect(page.getByRole("status")).toContainText("Pushed to hermes");
  await expect(page.getByRole("log", { name: "Posts" })).toContainText("on it");

  // Delivery for this channel.
  await page.getByTestId("delivery").getByRole("radio", { name: "pull" }).click();
  await expect(page.getByTestId("delivery").getByRole("radio", { name: "pull" })).toHaveAttribute("aria-checked", "true");
});

test("warden: badges, verdicts, quarantine and release", async ({ page }) => {
  await page.goto("/fleet?mock=demo");
  const heph = page.locator('[data-room="hephaestus"]');
  await expect(heph).toContainText("quarantined");
  await expect(heph).toContainText("warden critical");

  await page.goto("/rooms/acme/hephaestus?tab=activity");
  await expect(page.getByTestId("quarantine-banner")).toContainText(".ssh");
  const verdicts = page.getByTestId("verdict");
  await expect(verdicts.first()).toContainText("Gateway guard");
  await verdicts.nth(1).getByRole("button", { name: "Dismiss" }).click();
  await expect(verdicts.nth(1)).toContainText("dismissed by admin");
  await page.getByTestId("quarantine-banner").getByLabel(/Why it is safe/).fill("Key rotated; reviewed the transcript");
  await page.getByTestId("quarantine-banner").getByRole("button", { name: "Release" }).click();
  await expect(page.getByTestId("quarantine-banner")).toHaveCount(0);

  // Quarantine another room by hand.
  await page.goto("/rooms/acme/hecate?tab=activity");
  await page.getByRole("button", { name: "Quarantine now" }).click();
  await page.getByRole("dialog").getByLabel(/Reason/).fill("Touching datasets it shouldn't");
  await page.getByRole("dialog").getByRole("button", { name: "Quarantine" }).click();
  await expect(page.getByTestId("quarantine-banner")).toContainText("Touching datasets");
});
