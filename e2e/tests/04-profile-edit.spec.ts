import { expect, test } from "@playwright/test";
import { readFileSync } from "node:fs";
import { PROFILE, ensureSetupComplete } from "../support/setup";
import { workspaceFromEnv } from "../support/workspace";

const ws = workspaceFromEnv();

/**
 * Editing a profile through the web UI.
 *
 * This exists because saving this form used to destroy data. The handler
 * rebuilt the profile from the form's inputs alone, so any field without a
 * control - previous addresses, other email addresses, name variants, phone
 * numbers, date of birth - was reset to empty on every save. Those are
 * exactly the values that help a broker find the right record to delete, and
 * losing them was invisible: the form showed a successful save.
 *
 * Asserting through the browser matters here in a way a handler test can't
 * match, because half the fix is that the values are *shown* in the form. A
 * value the user cannot see is one they will unknowingly wipe.
 */
test.describe.configure({ mode: "serial" });

const ADDRESSES = ["1 Old Street, Springfield", "2 Newer Road, Apt 5, Riga"];
const OTHER_EMAILS = ["old.address@example.com", "work@example.com"];

test.beforeAll(async ({ browser }) => {
  const page = await browser.newPage();
  await ensureSetupComplete(page);
  await page.close();
});

test("previous addresses and other emails can be added", async ({ page }) => {
  await page.goto("/settings");
  await page.getByRole("link", { name: "Edit" }).first().click();
  await expect(page).toHaveURL(/\/settings\/profiles\/[^/]+\/edit$/);

  // Both start empty on a profile that came from the setup wizard.
  await expect(page.locator("#previous_addresses")).toHaveValue("");
  await expect(page.locator("#additional_emails")).toHaveValue("");

  await page.fill("#previous_addresses", ADDRESSES.join("\n"));
  await page.fill("#additional_emails", OTHER_EMAILS.join("\n"));
  await page.getByRole("button", { name: /Save/ }).click();
  await expect(page).toHaveURL(/\/settings$/);

  // Written through to disk, not merely held in memory.
  const config = readFileSync(ws.configPath, "utf8");
  for (const value of [...ADDRESSES, ...OTHER_EMAILS]) {
    expect(config, `${value} should be saved to config.yaml`).toContain(value);
  }
  // An address containing commas must be one entry, not three.
  expect(config).toContain("2 Newer Road, Apt 5, Riga");
});

test("existing entries are shown again for editing", async ({ page }) => {
  await page.goto("/settings");
  await page.getByRole("link", { name: "Edit" }).first().click();

  // The other half of the fix. If the form came back empty, saving would
  // silently discard everything the previous test just entered.
  await expect(page.locator("#previous_addresses")).toHaveValue(ADDRESSES.join("\n"));
  await expect(page.locator("#additional_emails")).toHaveValue(OTHER_EMAILS.join("\n"));
});

test("an existing entry can be changed and another removed", async ({ page }) => {
  await page.goto("/settings");
  await page.getByRole("link", { name: "Edit" }).first().click();

  // Edit the first address, drop the second entirely.
  await page.fill("#previous_addresses", "1 Old Street, Apt 9, Springfield");
  // Remove one of the two emails.
  await page.fill("#additional_emails", OTHER_EMAILS[1]);
  await page.getByRole("button", { name: /Save/ }).click();
  await expect(page).toHaveURL(/\/settings$/);

  await page.goto("/settings");
  await page.getByRole("link", { name: "Edit" }).first().click();
  await expect(page.locator("#previous_addresses")).toHaveValue("1 Old Street, Apt 9, Springfield");
  await expect(page.locator("#additional_emails")).toHaveValue(OTHER_EMAILS[1]);

  const config = readFileSync(ws.configPath, "utf8");
  expect(config).toContain("1 Old Street, Apt 9, Springfield");
  expect(config, "the removed address should be gone from config.yaml").not.toContain("2 Newer Road");
  expect(config, "the removed email should be gone from config.yaml").not.toContain(OTHER_EMAILS[0]);
});

// The regression proper: an edit that touches nothing must not destroy the
// list fields. Before the fix, opening the form and pressing Save was enough
// to erase them.
test("saving without changing anything preserves the lists", async ({ page }) => {
  await page.goto("/settings");
  await page.getByRole("link", { name: "Edit" }).first().click();
  await page.getByRole("button", { name: /Save/ }).click();
  await expect(page).toHaveURL(/\/settings$/);

  const config = readFileSync(ws.configPath, "utf8");
  expect(config, "a no-op save wiped the previous address").toContain("1 Old Street, Apt 9, Springfield");
  expect(config, "a no-op save wiped the other email address").toContain(OTHER_EMAILS[1]);
  // And the identity fields the form does carry are still intact.
  expect(config).toContain(PROFILE.lastName);
  expect(config).toContain(PROFILE.email);
});

test("the profile edit form still validates", async ({ page }) => {
  await page.goto("/settings");
  await page.getByRole("link", { name: "Edit" }).first().click();

  await page.fill("#last_name", "");
  await page.getByRole("button", { name: /Save/ }).click();

  // Same reasoning as the other negative tests: assert what the browser
  // reports about itself rather than reaching in to strip the constraint.
  await expect(page.locator("#last_name:invalid")).toBeVisible();
  await expect(page).toHaveURL(/\/settings\/profiles\/[^/]+\/edit$/);
});
