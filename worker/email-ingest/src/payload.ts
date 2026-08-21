import type { Email as ParsedEmail, Address } from "postal-mime";

/**
 * The JSON contract with internal/ingest.Message on the Ziga side.
 *
 * The fixtures in ../../internal/ingest/testdata/corpus are literal instances
 * of this type, and parse.test.ts asserts this builder reproduces them from
 * the matching .eml. That is what stops the two sides drifting: a change to
 * the shape fails here rather than silently changing what gets filtered in Go.
 */
export const PAYLOAD_VERSION = 1;

export interface Identity {
  name: string;
  address: string;
}

export interface AttachmentMeta {
  filename: string;
  mime_type: string;
  size: number;
}

export interface Payload {
  v: number;
  /**
   * The ENVELOPE recipient, not the To: header. A capture address that was
   * BCC'd has a To: header naming someone else entirely, so resolving the
   * tenant from the header would fail — or worse, resolve to the wrong tenant.
   */
  to: string;
  envelope_from: string;
  message_id: string;
  from: Identity;
  reply_to: Identity;
  subject: string;
  date: string;
  received_at: string;
  headers: Record<string, string[]>;
  text: string;
  html: string;
  attachments: AttachmentMeta[];
  raw_size: number;
  truncated: boolean;
  worker_event: string;
}

export const WORKER_EVENT_SIZE_REJECTED = "size_rejected";
export const WORKER_EVENT_PARSE_FAILED = "parse_failed";

/**
 * Headers the Go filter pipeline actually reads, and nothing else.
 *
 * A whitelist rather than everything, for two reasons: it bounds the payload
 * so an inflated header set cannot be used to push us past the body limit, and
 * it keeps the contract explicit — a filter that needs a new header has to add
 * it here, where the reason is visible.
 */
const HEADER_WHITELIST = new Set([
  "auto-submitted",
  "precedence",
  "x-autoreply",
  "x-autorespond",
  "x-auto-response-suppress",
  "list-unsubscribe",
  "list-id",
  "list-post",
  "x-forwarded-for",
  "x-forwarded-to",
  "delivered-to",
  "resent-from",
  "resent-date",
  "in-reply-to",
  "references",
  "content-type",
  "x-mailer",
  "x-spam-flag",
  "return-path",
  "authentication-results",
]);

const MAX_HEADER_VALUE_CHARS = 1000;
const MAX_HEADER_OCCURRENCES = 10;
/**
 * Body text is capped well above what the Go side will send to the model, so
 * truncation decisions stay on the Ziga side where they are visible to the
 * user as a review flag.
 */
const MAX_BODY_CHARS = 200_000;

function identity(a: Address | undefined | null): Identity {
  if (!a) return { name: "", address: "" };
  return { name: a.name ?? "", address: (a.address ?? "").toLowerCase() };
}

function collectHeaders(parsed: ParsedEmail): Record<string, string[]> {
  const out: Record<string, string[]> = {};
  for (const h of parsed.headers ?? []) {
    const key = (h.key ?? "").toLowerCase();
    if (!HEADER_WHITELIST.has(key)) continue;
    const values = (out[key] ??= []);
    if (values.length >= MAX_HEADER_OCCURRENCES) continue;
    values.push(String(h.value ?? "").slice(0, MAX_HEADER_VALUE_CHARS));
  }
  return out;
}

function isoOr(value: Date | string | undefined | null, fallback: string): string {
  if (!value) return fallback;
  const d = value instanceof Date ? value : new Date(value);
  return Number.isNaN(d.getTime()) ? fallback : d.toISOString();
}

export interface EnvelopeInput {
  to: string;
  from: string;
  rawSize: number;
  receivedAt?: Date;
}

/** Builds the JSON payload from the envelope and the parsed message. */
export function buildPayload(env: EnvelopeInput, parsed: ParsedEmail): Payload {
  const receivedAt = (env.receivedAt ?? new Date()).toISOString();
  const text = (parsed.text ?? "").slice(0, MAX_BODY_CHARS);
  const html = (parsed.html ?? "").slice(0, MAX_BODY_CHARS);

  return {
    v: PAYLOAD_VERSION,
    to: env.to.toLowerCase(),
    envelope_from: env.from,
    message_id: parsed.messageId ?? "",
    from: identity(parsed.from),
    reply_to: identity(parsed.replyTo?.[0]),
    subject: parsed.subject ?? "",
    date: isoOr(parsed.date, receivedAt),
    received_at: receivedAt,
    headers: collectHeaders(parsed),
    text,
    html,
    // Metadata only in v1: attachments are out of scope, but their presence
    // distinguishes "a lead with a logo attached" from "a scan and no text",
    // which the Go filters care about.
    attachments: (parsed.attachments ?? []).map((a) => ({
      filename: a.filename ?? "",
      mime_type: a.mimeType ?? "",
      size: a.content instanceof ArrayBuffer ? a.content.byteLength : 0,
    })),
    raw_size: env.rawSize,
    truncated: (parsed.text ?? "").length > MAX_BODY_CHARS || (parsed.html ?? "").length > MAX_BODY_CHARS,
    worker_event: "",
  };
}

/**
 * Builds a headers-only payload for a message we could not read.
 *
 * The worker never drops what it cannot handle. An oversized message is
 * usually a real lead with a large attachment, and a parse failure is a real
 * message from a client we mishandled — both are reported so the user sees
 * that something arrived, rather than the lead vanishing.
 */
export function buildFailurePayload(env: EnvelopeInput, event: string, subject = ""): Payload {
  const receivedAt = (env.receivedAt ?? new Date()).toISOString();
  return {
    v: PAYLOAD_VERSION,
    to: env.to.toLowerCase(),
    envelope_from: env.from,
    message_id: "",
    from: { name: "", address: env.from.toLowerCase() },
    reply_to: { name: "", address: "" },
    subject,
    date: receivedAt,
    received_at: receivedAt,
    headers: {},
    text: "",
    html: "",
    attachments: [],
    raw_size: env.rawSize,
    truncated: false,
    worker_event: event,
  };
}
