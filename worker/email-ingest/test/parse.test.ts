import { describe, it, expect } from "vitest";
import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import PostalMime from "postal-mime";
import { buildPayload, type Payload } from "../src/payload";

/**
 * The cross-language corpus contract.
 *
 * ../../internal/ingest/testdata/corpus holds triples: a raw .eml, the
 * ingest.Message .json the Go filters run against, and the expected outcome.
 * Go never parses the .eml — reimplementing MIME parsing there would mean a
 * second parser with different bugs. Instead THIS test asserts that parsing
 * the .eml and building a payload reproduces the committed .json.
 *
 * That is what makes the corpus a contract rather than two fixture sets that
 * drift: a change to this builder's output fails here, loudly, instead of
 * quietly changing what gets filtered in production.
 */
const CORPUS = join(import.meta.dirname, "..", "..", "..", "internal", "ingest", "testdata", "corpus");

function fixtureNames(): string[] {
  return readdirSync(CORPUS)
    .filter((f) => f.endsWith(".json") && !f.endsWith(".want.json"))
    .map((f) => f.slice(0, -".json".length))
    .sort();
}

function loadExpected(name: string): Payload {
  return JSON.parse(readFileSync(join(CORPUS, `${name}.json`), "utf8"));
}

/** Fields the worker derives from the delivery, not the message. */
function envelopeFor(expected: Payload, rawSize: number) {
  return {
    to: expected.to,
    from: expected.envelope_from,
    rawSize,
    receivedAt: new Date(expected.received_at),
  };
}

describe("payload building", () => {
  const names = fixtureNames();

  it("finds the shared corpus", () => {
    expect(names.length).toBeGreaterThan(0);
  });

  for (const name of names) {
    const expected = loadExpected(name);

    // Failure fixtures never went through the parser: the worker reports them
    // from the envelope alone.
    if (expected.worker_event) continue;

    it(`reproduces ${name}.json from ${name}.eml`, async () => {
      const raw = readFileSync(join(CORPUS, `${name}.eml`));
      const parsed = await PostalMime.parse(raw);
      const got = buildPayload(envelopeFor(expected, expected.raw_size), parsed);

      // The identity fields decide which tenant a lead lands in and who it is
      // attributed to, so they are compared exactly.
      expect(got.to).toBe(expected.to);
      expect(got.from.address).toBe(expected.from.address);
      expect(got.subject).toBe(expected.subject);
      expect(got.message_id).toBe(expected.message_id);

      // Body text must survive intact — this is what reaches the model.
      expect(normalise(got.text)).toBe(normalise(expected.text));

      // Every whitelisted header the Go filters read must be present. Missing
      // one silently disables the filter that depends on it.
      for (const key of Object.keys(expected.headers)) {
        expect(got.headers[key], `header ${key} missing from the built payload`).toBeDefined();
      }

      expect(got.attachments.length).toBe(expected.attachments.length);
      expect(got.v).toBe(expected.v);
    });
  }
});

describe("header whitelist", () => {
  it("keeps the headers the filters read and drops the rest", async () => {
    const raw = [
      "From: Ada <ada@lumen.studio>",
      "To: <lead-abc@in.example.com>",
      "Subject: Test",
      "Precedence: bulk",
      "List-Unsubscribe: <https://x.example/u>",
      "X-Enormous-Tracking-Blob: " + "A".repeat(50_000),
      "X-Some-Other-Header: irrelevant",
      "Content-Type: text/plain",
      "",
      "Body text here.",
      "",
    ].join("\r\n");

    const parsed = await PostalMime.parse(raw);
    const got = buildPayload({ to: "lead-abc@in.example.com", from: "ada@lumen.studio", rawSize: raw.length }, parsed);

    expect(got.headers["precedence"]).toEqual(["bulk"]);
    expect(got.headers["list-unsubscribe"]).toBeDefined();
    // An unbounded header must not be a way to inflate the payload past the
    // server's body limit and get the message rejected.
    expect(got.headers["x-enormous-tracking-blob"]).toBeUndefined();
    expect(got.headers["x-some-other-header"]).toBeUndefined();
  });

  it("truncates an oversized whitelisted header", async () => {
    const raw = ["From: Ada <ada@lumen.studio>", "Subject: T", "Precedence: " + "b".repeat(50_000), "", "Body.", ""].join(
      "\r\n",
    );
    const parsed = await PostalMime.parse(raw);
    const got = buildPayload({ to: "lead-abc@in.example.com", from: "ada@lumen.studio", rawSize: raw.length }, parsed);
    expect(got.headers["precedence"]![0]!.length).toBeLessThanOrEqual(1000);
  });
});

describe("envelope handling", () => {
  it("uses the envelope recipient, not the To: header", async () => {
    // A capture address that was BCC'd has a To: header naming someone else.
    // Resolving the tenant from the header would deliver the lead to the
    // wrong account, or to none.
    const raw = [
      "From: Ada <ada@lumen.studio>",
      "To: <someone-else@example.com>",
      "Subject: Enquiry",
      "",
      "Please quote for a website.",
      "",
    ].join("\r\n");

    const parsed = await PostalMime.parse(raw);
    const got = buildPayload({ to: "lead-Secret@in.example.com", from: "ada@lumen.studio", rawSize: raw.length }, parsed);

    expect(got.to).toBe("lead-secret@in.example.com");
    expect(got.to).not.toContain("someone-else");
  });
});

function normalise(s: string): string {
  return s.replace(/\r\n/g, "\n").trim();
}
