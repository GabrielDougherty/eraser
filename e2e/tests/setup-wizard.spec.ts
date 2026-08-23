import { expect, test } from "@playwright/test";
import { existsSync, readFileSync } from "node:fs";
import { readManifest } from "../support/captured";
import { workspaceFromEnv } from "../support/workspace";

const ws = workspaceFromEnv();

export const PROFILE = {
  firstName: "Test",
  lastName: "Persoon",
  email: "e2e@example.com",
  city: "Riga",
  country: "Latvia",
  smtpUsername: "e2e@example.com",
  smtpPassword: "fixture-app-password",
};

test.describe.configure({ mode: "serial" });

test("a fresh install completes the setup wizard", async ({ page }) => {
  // If this fails, the workspace was reused and nothing below means what it
  // claims to. Fail here with a clear reason rather than somewhere confusing.
  expect(
    existsSync(ws.configPath),
    `${ws.configPath} already exists - this run's workspace is not fresh`,
  ).toBe(false);

  await page.goto("/");
  await expect(page).toHaveURL(/\/setup$/);
  await expect(page.getByRole("heading", { name: "Welcome to Eraser" })).toBeVisible();

  // Capture mode must be visibly on. Without this assertion the whole suite
  // could pass while pointed at a real mail server.
  await expect(page.getByText("Capture mode")).toBeVisible();

  await page.getByRole("link", { name: /Get Started/ }).click();
  await expect(page).toHaveURL(/\/setup\/profile$/);

  // Negative case first: an incomplete profile must not advance.
  await page.fill("#first_name", PROFILE.firstName);
  await page.fill("#email", PROFILE.email);
  await page.getByRole("button", { name: /Continue/ }).click();
  await expect(page).toHaveURL(/\/setup\/profile$/);

  await page.fill("#last_name", PROFILE.lastName);
  await page.fill("#city", PROFILE.city);
  await page.fill("#country", PROFILE.country);
  await page.getByRole("button", { name: /Continue/ }).click();

  // Reaching the email step is the real assertion here. The session cookie is
  // set on the response to this POST while the profile is saved server-side,
  // and CSRF is enforced on the form - if either breaks, this bounces back to
  // /setup/profile with the form cleared. No Go test can catch that today,
  // because none of them start a real listener.
  await expect(page).toHaveURL(/\/setup\/email$/);

  // Host, port and TLS are hidden inputs defaulted to Gmail, so only these two
  // are typed - the same thing a real person does.
  await page.fill("#smtp_username", PROFILE.smtpUsername);
  await page.fill("#smtp_password", PROFILE.smtpPassword);
  await page.getByRole("button", { name: /Continue/ }).click();
  await expect(page).toHaveURL(/\/setup\/test$/);

  // The wizard's test send builds its own config from the session rather than
  // from disk, so this passing proves the injected sender factory reaches even
  // that path. Against a real sender it would try to dial smtp.gmail.com with
  // a fixture password.
  await page.getByRole("button", { name: /Send Test Email/ }).click();
  await expect(page.locator("#test-result")).toContainText("Success", { timeout: 30_000 });

  const afterTestSend = readManifest(ws.captureDir);
  expect(afterTestSend).toHaveLength(1);
  expect(afterTestSend[0].to).toBe(PROFILE.email);
  expect(afterTestSend[0].subject).toBe("Eraser Test Email");

  // Exact: the page also offers "Skip and Complete Setup" below, and this is
  // specifically the link the success fragment adds - the path a person takes
  // after a test send actually worked.
  await page.getByRole("link", { name: "Complete Setup", exact: true }).click();
  await expect(page).toHaveURL(/\/setup\/complete$/);
  await expect(page.getByRole("heading", { name: /You're All Set/ })).toBeVisible();
  await expect(page.getByText(PROFILE.firstName)).toBeVisible();

  // Assert the config the wizard actually wrote. Plain string checks rather
  // than a YAML dependency. `template: gdpr` is this fork's deliberate default
  // and worth pinning from the outside.
  expect(existsSync(ws.configPath)).toBe(true);
  const written = readFileSync(ws.configPath, "utf8");
  expect(written).toContain("template: gdpr");
  expect(written).toContain("provider: smtp");
  expect(written).toContain(PROFILE.smtpUsername);
  expect(written).toContain(PROFILE.lastName);
});

test("the dashboard is reachable once configured", async ({ page }) => {
  await page.goto("/");
  await expect(page).toHaveURL(/127\.0\.0\.1:\d+\/$/);
  await expect(page.getByText(`Welcome back, ${PROFILE.firstName}`)).toBeVisible();

  // The fixture, not the shipped 763-broker database.
  await expect(page.getByText("763")).toHaveCount(0);
});
