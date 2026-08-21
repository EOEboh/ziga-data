import { describe, it, expect } from "vitest";
import { sign, signingString, SIGNATURE_SCHEME } from "../src/sign";

/**
 * The known-answer vector, shared with internal/ingest/hmac_test.go.
 *
 * This is the single most important test in the worker. The Go side verifies
 * what this code signs, and two implementations of one scheme drift silently.
 * When they do, every delivery 401s and leads simply stop arriving — there is
 * no other symptom, and nothing in the product looks broken.
 *
 * If you change the scheme, both suites fail together, which is the point.
 */
const SECRET = "test-secret-at-least-32-chars-long!!";
const TIMESTAMP = "1755000000";
const BODY = '{"v":1,"to":"lead-abc@in.example.com"}';
const BODY_HASH = "fa69fbeccb120cc4d8b09441801f3ff8f2e8ac276dd5d31e0abc21ed43ca930c";
const EXPECTED_SIGNATURE = "v1=_cUrPMjZK2McM5nn9MeWGfrjcOQjDo7Q6g5bkwC0D4g";

const bytes = (s: string) => new TextEncoder().encode(s);

describe("signing", () => {
  it("produces the canonical signing string Go expects", async () => {
    const got = await signingString(TIMESTAMP, bytes(BODY));
    expect(got).toBe(`${SIGNATURE_SCHEME}:${TIMESTAMP}:${BODY_HASH}`);
  });

  it("matches the Go known-answer vector", async () => {
    const got = await sign(SECRET, TIMESTAMP, bytes(BODY));
    expect(got).toBe(EXPECTED_SIGNATURE);
  });

  it("keeps the signing string a fixed length regardless of body size", async () => {
    const small = await signingString(TIMESTAMP, bytes("x"));
    const large = await signingString(TIMESTAMP, new Uint8Array(1024 * 1024));
    expect(large.length).toBe(small.length);
  });

  it("changes the signature when a single body byte changes", async () => {
    const original = await sign(SECRET, TIMESTAMP, bytes(BODY));
    const tampered = await sign(SECRET, TIMESTAMP, bytes(BODY.replace("lead-abc", "lead-abd")));
    expect(tampered).not.toBe(original);
  });

  it("changes the signature when the timestamp changes", async () => {
    const original = await sign(SECRET, TIMESTAMP, bytes(BODY));
    const later = await sign(SECRET, "1755000001", bytes(BODY));
    expect(later).not.toBe(original);
  });

  it("emits unpadded base64url, matching Go's RawURLEncoding", async () => {
    const got = await sign(SECRET, TIMESTAMP, bytes(BODY));
    const mac = got.slice("v1=".length);
    expect(mac).not.toContain("=");
    expect(mac).not.toContain("+");
    expect(mac).not.toContain("/");
  });
});
