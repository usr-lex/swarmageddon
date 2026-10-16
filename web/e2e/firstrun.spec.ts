import { expect, test } from "@playwright/test";

const TOKEN = "MOCKTOKENMOCKTOKENMOCKTOKENMOCKTO";

test("setup, first-run wizard, workspace wizard, fleet, approve", async ({ page }) => {
  await page.goto("/?mock=1");

  // Setup: token, then the local admin.
  await expect(page).toHaveURL(/\/setup$/);
  await expect(page.getByTestId("ca-fingerprint")).toContainText("SHA256:");
  await page.getByLabel("Setup token").fill("wrong");
  await page.getByRole("button", { name: "Continue" }).click();
  await expect(page.getByRole("alert")).toContainText("not right");
  await page.getByLabel("Setup token").fill(TOKEN);
  await page.getByRole("button", { name: "Continue" }).click();
  await page.getByLabel("Email").fill("admin@example.com");
  await page.getByLabel("Password", { exact: true }).fill("correct horse battery");
  await page.getByLabel("Confirm password").fill("correct horse battery");
  await page.getByRole("button", { name: "Create admin" }).click();

  // Wizard 1: preflight.
  await expect(page).toHaveURL(/\/wizard$/);
  await expect(page.getByRole("list", { name: "Preflight checks" })).toContainText("NetworkPolicy enforcement");
  await expect(page.getByTestId("room-estimate")).toHaveText("7");
  await page.getByLabel(/Privileged/).check();
  const cont = page.getByRole("button", { name: "Continue", exact: true });
  await expect(cont).toBeDisabled();
  await page.getByLabel(/I understand that rooms will not be isolated/).check();
  await expect(cont).toBeEnabled();
  await page.getByLabel(/User namespaces/).check();
  await cont.click();

  // Wizard 2: sign-in, local accounts only.
  await page.getByRole("button", { name: "Continue with local accounts only" }).click();

  // Wizard 3: platform services.
  await page.getByRole("button", { name: "Add provider" }).click();
  await page.getByLabel("Name", { exact: true }).fill("anthropic");
  await page.getByLabel("API key").fill("sk-test");
  await page.getByRole("button", { name: "Deploy services" }).click();
  await expect(page.getByTestId("service-PostgresReady")).toBeVisible();
  await expect(page.getByTestId("service-RegistryCacheReady")).toHaveAttribute("data-state", "ready", { timeout: 15_000 });
  await page.getByRole("button", { name: "Continue", exact: true }).click();

  // Wizard 4: hostname, skipped.
  await expect(page.getByRole("heading", { name: "Trust the platform CA" })).toBeVisible();
  await page.getByRole("button", { name: "Skip this step" }).click();

  // Workspace wizard.
  await expect(page).toHaveURL(/\/workspaces\/new$/);
  await page.getByLabel("Name", { exact: true }).fill("acme");
  await page.getByLabel("Forgejo organization").fill("acme");
  await page.getByRole("button", { name: "Next" }).click();
  await page.getByLabel("How many").fill("3");
  await expect(page.getByTestId("agent-row")).toHaveCount(3);
  await expect(page.getByTestId("agent-row").first().getByLabel("Name")).toHaveValue("athena");
  await page.getByTestId("agent-row").nth(2).getByLabel("CLI").selectOption("ore");
  await page.getByRole("button", { name: "Next" }).click();
  await expect(page.getByTestId("fit")).toContainText("Fits");
  await page.getByRole("button", { name: "Next" }).click(); // repositories
  await page.getByRole("button", { name: "Next" }).click(); // groups
  await page.getByLabel("New group").fill("clean");
  await page.getByRole("button", { name: "Add group" }).click();
  await page.getByLabel("athena's group").selectOption("clean");
  await expect(page.getByRole("img")).toBeVisible();
  await page.getByRole("button", { name: "Next" }).click(); // notifications
  await page.getByRole("button", { name: "Next" }).click(); // review
  const plan = page.getByRole("list", { name: "Planned changes" });
  await expect(plan).toContainText("Room athena");
  await expect(plan).toContainText("RunnerPool default");
  await page.getByRole("button", { name: "Apply" }).click();
  await expect(page.getByTestId("progress-athena")).toHaveAttribute("data-phase", "Running", { timeout: 15_000 });
  await page.getByRole("link", { name: /Go to the fleet/ }).click();

  // Fleet: the room waiting on a human sorts first.
  await expect(page).toHaveURL(/\/fleet$/);
  const cards = page.getByTestId("room-card");
  await expect(cards).toHaveCount(3);
  await expect(cards.first()).toHaveAttribute("data-presence", "waitingOnHuman", { timeout: 10_000 });
  await expect(page.getByTestId("approvals-badge")).toContainText("1");

  // Approve.
  await page.getByRole("link", { name: /Approvals/ }).click();
  const approval = page.getByTestId("approval").first();
  await expect(approval).toContainText("git push origin feature/setup");
  await approval.getByRole("button", { name: "Allow once" }).click();
  await expect(page.getByText("Nothing is waiting for you.")).toBeVisible();
  await page.getByRole("link", { name: "Fleet" }).click();
  await expect(cards.first()).not.toHaveAttribute("data-presence", "waitingOnHuman");
});

test("push deep link opens one approval; deny with a message", async ({ page }) => {
  await page.goto("/approvals/01JDEMOAPPROVAL0000000001?mock=demo");
  const a = page.locator('[data-id="01JDEMOAPPROVAL0000000001"]');
  await expect(a).toContainText("npm install left-pad");
  await a.getByRole("button", { name: "Deny…" }).click();
  await a.getByLabel("Message to the agent").fill("Write it yourself.");
  await a.getByRole("button", { name: "Deny", exact: true }).click();
  await expect(a.getByRole("status")).toContainText("Answered (deny)");
});

test("room page: tabs and lifecycle confirmation", async ({ page }) => {
  await page.goto("/rooms/acme/athena?mock=demo");
  await expect(page.getByRole("heading", { name: /Athena/ })).toBeVisible();
  await expect(page.getByTestId("terminal-agent")).toBeVisible();
  await page.getByRole("tab", { name: "Storage" }).click();
  await page.getByRole("button", { name: "Prune build cache" }).click();
  await expect(page.getByText(/Reclaimed/)).toBeVisible();
  await page.getByRole("tab", { name: "Logs" }).click();
  await expect(page.getByRole("log", { name: "Logs" })).toContainText("supervisor");
  await page.getByRole("button", { name: "Recreate" }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByLabel(/Also delete the home volume/).check();
  const go = dialog.getByRole("button", { name: "Delete home and recreate" });
  await expect(go).toBeDisabled();
  await dialog.getByLabel(/Type athena to confirm/).fill("athena");
  await go.click();
  await expect(dialog).toBeHidden();
});
