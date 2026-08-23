import PostalMime from "postal-mime";
import { buildPayload, buildFailurePayload, WORKER_EVENT_PARSE_FAILED, WORKER_EVENT_SIZE_REJECTED } from "./payload";
import { postToZiga } from "./post";

export interface Env {
  ZIGA_INGEST_URL: string;
  /** wrangler secret — never a var, never committed. */
  ZIGA_INGEST_SECRET: string;
  MAX_RAW_BYTES: string;
  /**
   * A verified destination address that receives mail we could not deliver to
   * Ziga. Set as a wrangler SECRET rather than a var: it is a real mailbox and
   * this repository is public. Optional because unset is a supported (if
   * degraded) state — the fallback becomes a loud log; see the end of email().
   */
  FALLBACK_ADDRESS?: string;
}

/** Cloudflare's own inbound ceiling, for context in the size check below. */
const CLOUDFLARE_INBOUND_LIMIT = 25 * 1024 * 1024;

export default {
  /**
   * Handles one inbound message.
   *
   * Deliberately thin: it parses MIME, signs, and posts. Every judgement about
   * whether a message is a lead lives in Ziga, where it is testable against a
   * fixture corpus and visible to the user when it goes wrong.
   *
   * Cloudflare does not retry this handler and has no queue behind it, so an
   * unhandled exception loses the mail outright. Everything below is arranged
   * so that cannot happen.
   */
  async email(message: ForwardableEmailMessage, env: Env): Promise<void> {
    const envelope = {
      to: message.to,
      from: message.from,
      rawSize: message.rawSize,
      receivedAt: new Date(),
    };

    const maxRaw = Number(env.MAX_RAW_BYTES) || 2 * 1024 * 1024;

    // Size check reads rawSize and never touches the stream, so an oversized
    // message costs nothing.
    //
    // It is reported rather than rejected. setReject() is a PERMANENT SMTP
    // error: using it here would bounce a real lead's email back to them
    // because they attached a large PDF. Instead the user sees "someone
    // emailed you a 30 MB file" in quarantine and can follow it up.
    if (message.rawSize > maxRaw) {
      console.warn(
        `oversized message: ${message.rawSize} bytes exceeds our ${maxRaw} limit ` +
          `(Cloudflare's own ceiling is ${CLOUDFLARE_INBOUND_LIMIT})`,
      );
      await deliver(env, message, buildFailurePayload(envelope, WORKER_EVENT_SIZE_REJECTED));
      return;
    }

    let payload;
    try {
      const parsed = await PostalMime.parse(message.raw);
      payload = buildPayload(envelope, parsed);
    } catch (err) {
      // A client we mishandle is still a real message. Report it with what we
      // know from the envelope rather than dropping it.
      console.error("MIME parse failed", err);
      payload = buildFailurePayload(envelope, WORKER_EVENT_PARSE_FAILED, message.headers.get("subject") ?? "");
    }

    await deliver(env, message, payload);
  },
} satisfies ExportedHandler<Env>;

/**
 * Posts to Ziga, falling back to a monitored mailbox when it cannot be
 * reached. Losing a lead silently is the worst failure available here, so the
 * fallback is the whole point of this function.
 */
async function deliver(
  env: Env,
  message: ForwardableEmailMessage,
  payload: ReturnType<typeof buildFailurePayload>,
): Promise<void> {
  const result = await postToZiga(env.ZIGA_INGEST_URL, env.ZIGA_INGEST_SECRET, payload);
  if (result.ok) {
    console.log(`delivered to ziga: ${result.outcome || "accepted"}`);
    return;
  }

  console.error(`ziga delivery failed after retries: status=${result.status} error=${result.error ?? ""}`);

  if (!env.FALLBACK_ADDRESS) {
    // Deliberately loud and deliberately not fatal. Throwing would not
    // preserve the mail either — Cloudflare does not retry — so all a throw
    // would add is a less legible log line.
    console.error(
      `NO FALLBACK CONFIGURED — this message is lost. to=${payload.to} from=${payload.envelope_from} ` +
        `message_id=${payload.message_id}. Set FALLBACK_ADDRESS to a verified destination address.`,
    );
    return;
  }

  try {
    // Only X- prefixed custom headers are permitted on forward().
    const headers = new Headers();
    headers.set("X-Ziga-Intended-To", payload.to);
    headers.set("X-Ziga-Failure", `status=${result.status} ${result.error ?? ""}`.slice(0, 200));
    await message.forward(env.FALLBACK_ADDRESS, headers);
    console.warn(`forwarded to fallback ${env.FALLBACK_ADDRESS}; replay it into Ziga by hand`);
  } catch (err) {
    // forward() throws when the address is not a VERIFIED destination, and
    // also for messages Cloudflare will not forward at all — either way the
    // safety net has become a second failure, which is why the runbook
    // verifies the address before this is ever deployed.
    console.error(
      `FALLBACK FORWARD FAILED — this message is lost. Is ${env.FALLBACK_ADDRESS} a verified ` +
        `destination address? error=${String(err)}`,
    );
  }
}
