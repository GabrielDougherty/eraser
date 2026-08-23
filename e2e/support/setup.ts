import type { Page } from "@playwright/test";

/**
 * Facts about fixtures/brokers.yaml that specs assert on. Kept here rather
 * than inline so adding a broker means changing one number, not hunting for
 * every "6 of 6" string across the suite - which is exactly what went wrong
 * the first time a broker was added.
 */
export const FIXTURE = {
  /** Total brokers in fixtures/brokers.yaml. */
  brokers: 7,
  /** Brokers with no address at all: skipped, never attempted. */
  noAddress: 2,
  /** Brokers with a malformed address: attempted, rejected by the sender. */
  brokenAddress: 1,
};

/** The identity the suite configures. Shared so specs can assert on it. */
export const PROFILE = {
  firstName: "Test",
  lastName: "Persoon",
  fullName: "Test Persoon",
  email: "e2e@example.com",
  city: "Riga",
  country: "Latvia",
  smtpUsername: "e2e@example.com",
  smtpPassword: "fixture-app-password",
};

/**
 * Walks the setup wizard if the app isn't configured yet, and does nothing if
 * it already is.
 *
 * Specs are numbered so the wizard spec runs first in a full run, which makes
 * this a no-op there. It earns its keep when running a later spec on its own
 * with --grep, which is what you want while debugging one failure - without
 * it, every send would 400 with "Email not configured" and the reason would
 * not be obvious.
 */
export async function ensureSetupComplete(page: Page): Promise<void> {
  await page.goto("/");
  if (!page.url().includes("/setup")) return;

  await page.goto("/setup/profile");
  await page.fill("#first_name", PROFILE.firstName);
  await page.fill("#last_name", PROFILE.lastName);
  await page.fill("#email", PROFILE.email);
  await page.fill("#city", PROFILE.city);
  await page.fill("#country", PROFILE.country);
  await page.getByRole("button", { name: /Continue/ }).click();

  await page.fill("#smtp_username", PROFILE.smtpUsername);
  await page.fill("#smtp_password", PROFILE.smtpPassword);
  await page.getByRole("button", { name: /Continue/ }).click();

  // Skip the test send here; the wizard spec covers it in detail, and this
  // path only needs the app configured.
  await page.getByRole("link", { name: "Skip and Complete Setup" }).click();
  await page.waitForURL(/\/setup\/complete$/);
}
