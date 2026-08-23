import { existsSync, readFileSync, readdirSync } from "node:fs";
import path from "node:path";

/**
 * One entry from the capture directory's manifest.jsonl, matching
 * email.CapturedMessage on the Go side.
 */
export interface CapturedMessage {
  seq: number;
  to: string;
  from: string;
  subject: string;
  file: string;
  message_id: string;
  sent_at: string;
}

/**
 * Reads everything captured so far, in send order.
 *
 * The manifest is what makes assertions cheap: structured fields without a
 * MIME parser in the test suite. The .eml files alongside it hold the exact
 * bytes that would have gone to the SMTP server, for body assertions.
 */
export function readManifest(captureDir: string): CapturedMessage[] {
  const manifest = path.join(captureDir, "manifest.jsonl");
  if (!existsSync(manifest)) return [];
  return readFileSync(manifest, "utf8")
    .split("\n")
    .filter((line) => line.trim() !== "")
    .map((line) => JSON.parse(line) as CapturedMessage);
}

/** Raw bytes of one captured message, as a string. */
export function readCapturedBody(captureDir: string, file: string): string {
  return readFileSync(path.join(captureDir, file), "utf8");
}

/** The .eml body for the first message sent to a given recipient. */
export function bodyFor(captureDir: string, recipient: string): string {
  const entry = readManifest(captureDir).find((m) => m.to === recipient);
  if (!entry) {
    const seen = readManifest(captureDir).map((m) => m.to);
    throw new Error(`no captured message for ${recipient}; captured: ${JSON.stringify(seen)}`);
  }
  return readCapturedBody(captureDir, entry.file);
}

/** Every recipient captured so far, in send order. */
export function capturedRecipients(captureDir: string): string[] {
  return readManifest(captureDir).map((m) => m.to);
}

/** .eml filenames on disk, for cross-checking against the manifest. */
export function capturedFiles(captureDir: string): string[] {
  if (!existsSync(captureDir)) return [];
  return readdirSync(captureDir)
    .filter((f) => f.endsWith(".eml"))
    .sort();
}
