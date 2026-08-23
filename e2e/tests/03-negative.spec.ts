import { expect, test } from "@playwright/test";
import { ensureSetupComplete } from "../support/setup";

/**
 * Rejection paths.
 *
 * The happy-path specs prove the app works when everything is right. These
 * prove it says no when something is wrong - the half that tends to rot
 * unnoticed, because nobody exercises it by hand.
 *
 * Scope note: these assert what a person in a browser can actually reach.
 * Server-side field validation is deliberately *not* tested here - forcing a
 * request past the form's own constraints would mean editing the DOM, which
 * no user can do, and it is already covered where it belongs, against the
 * handler directly (internal/web/handlers_profile_test.go).
 *
 * Runs last: completing the wizard clears the setup session, which is exactly
 * the state the step guards below need.
 */
test.describe.configure({ mode: "serial" });

test.beforeAll(async ({ browser }) => {
  const page = await browser.newPage();
  await ensureSetupComplete(page);
  await page.close();
});

test("an incomplete profile cannot be submitted", async ({ page }) => {
  await page.goto("/setup/profile");

  await page.fill("#first_name", "Test");
  await page.fill("#email", "someone@example.com");
  await page.getByRole("button", { name: /Continue/ }).click();

  // The browser refuses to submit and marks the offending field. Asserting
  // via :invalid keeps this observational - we check what the page reports
  // about itself, rather than reaching in to change it.
  await expect(page.locator("#last_name:invalid")).toBeVisible();
  await expect(page).toHaveURL(/\/setup\/profile$/);
});

test("a malformed email cannot be submitted", async ({ page }) => {
  await page.goto("/setup/profile");

  await page.fill("#first_name", "Test");
  await page.fill("#last_name", "Persoon");
  await page.fill("#email", "not-an-email");
  await page.getByRole("button", { name: /Continue/ }).click();

  await expect(page.locator("#email:invalid")).toBeVisible();
  await expect(page).toHaveURL(/\/setup\/profile$/);
});

test("wizard steps cannot be entered out of order", async ({ page }) => {
  // Completing setup clears the session, so these steps have nothing to
  // resume and must send you back rather than rendering a half-built form.
  // Typing a URL is something people genuinely do, so this is worth covering
  // from the browser rather than only against the handler.
  for (const [step, expected] of [
    ["/setup/email", /\/setup\/profile$/],
    ["/setup/test", /\/setup\/profile$/],
    ["/setup/complete", /\/setup$/],
  ] as const) {
    await page.goto(step);
    await expect(page, `${step} should not be reachable without a session`).toHaveURL(expected);
  }
});

test("a state-changing POST without a CSRF token is refused", async ({ page }) => {
  // Not a user journey - deliberately so. This is the one request shape a
  // person cannot produce through the UI but an attacker's page can: it
  // carries the victim's cookies and is missing only the token the real form
  // would submit. Nothing else in the suite would notice if CSRF protection
  // were switched off, since every other request goes through a real form.
  const response = await page.request.post("/setup/profile", {
    form: {
      first_name: "Attacker",
      last_name: "Injected",
      email: "attacker@example.invalid",
    },
    failOnStatusCode: false,
  });

  expect(response.status()).toBe(403);
});
