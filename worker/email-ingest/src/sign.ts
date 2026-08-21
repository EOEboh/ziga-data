/**
 * Request signing for the Ziga ingestion webhook.
 *
 * This is one half of a cross-language pair: internal/ingest/hmac.go verifies
 * what this produces. Two implementations of one scheme drift silently, and
 * the failure is total — every lead stops arriving, with nothing but 401s to
 * show for it. So both sides assert the same known-answer vector, and a change
 * to the scheme fails in both test suites together, on purpose.
 */

export const SIGNATURE_HEADER = "X-Ziga-Signature";
export const TIMESTAMP_HEADER = "X-Ziga-Timestamp";
export const SIGNATURE_SCHEME = "v1";

/**
 * The exact string that gets signed.
 *
 * The body is hashed rather than concatenated, so the signed string is a fixed
 * length whatever the payload size, and the fixed-width hex digest leaves no
 * way to shift bytes between the timestamp and the body while producing the
 * same string.
 */
export async function signingString(timestamp: string, body: Uint8Array): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", body as BufferSource);
  return `${SIGNATURE_SCHEME}:${timestamp}:${hex(new Uint8Array(digest))}`;
}

/** Produces the value for SIGNATURE_HEADER. */
export async function sign(secret: string, timestamp: string, body: Uint8Array): Promise<string> {
  const key = await crypto.subtle.importKey(
    "raw",
    new TextEncoder().encode(secret),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"],
  );
  const message = new TextEncoder().encode(await signingString(timestamp, body));
  const mac = await crypto.subtle.sign("HMAC", key, message as BufferSource);
  return `${SIGNATURE_SCHEME}=${base64url(new Uint8Array(mac))}`;
}

function hex(bytes: Uint8Array): string {
  let out = "";
  for (const b of bytes) out += b.toString(16).padStart(2, "0");
  return out;
}

/** base64url without padding, matching Go's base64.RawURLEncoding. */
function base64url(bytes: Uint8Array): string {
  let binary = "";
  for (const b of bytes) binary += String.fromCharCode(b);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}
