import { sign, SIGNATURE_HEADER, TIMESTAMP_HEADER } from "./sign";
import type { Payload } from "./payload";

const ATTEMPTS = 3;
const BACKOFF_MS = [250, 1000, 4000];
const TIMEOUT_MS = 10_000;

export interface PostResult {
  ok: boolean;
  status: number;
  /** Body discriminator from Ziga: accepted | duplicate | quarantined | ... */
  outcome: string;
  error?: string;
}

/**
 * POSTs a signed payload to Ziga, retrying transient failures.
 *
 * Retries cover network errors and 5xx only. A 401 will not fix itself — it
 * means the shared secret drifted — and retrying it three times just triples
 * the log noise while delaying the fallback that actually preserves the mail.
 */
export async function postToZiga(url: string, secret: string, payload: Payload): Promise<PostResult> {
  const body = new TextEncoder().encode(JSON.stringify(payload));
  let last: PostResult = { ok: false, status: 0, outcome: "", error: "no attempt made" };

  for (let attempt = 0; attempt < ATTEMPTS; attempt++) {
    if (attempt > 0) await sleep(jitter(BACKOFF_MS[attempt - 1]));

    // Signed per attempt: the timestamp is inside the signature, and a
    // signature minted before a four-second backoff would age towards the
    // skew window for no reason.
    const timestamp = Math.floor(Date.now() / 1000).toString();
    const signature = await sign(secret, timestamp, body);

    try {
      const resp = await fetch(url, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          [TIMESTAMP_HEADER]: timestamp,
          [SIGNATURE_HEADER]: signature,
        },
        body,
        signal: AbortSignal.timeout(TIMEOUT_MS),
      });

      const outcome = await readOutcome(resp);
      if (resp.ok) return { ok: true, status: resp.status, outcome };

      last = { ok: false, status: resp.status, outcome, error: `status ${resp.status}` };
      // 4xx is a decision, not a hiccup. Stop and let the fallback run.
      if (resp.status < 500) return last;
    } catch (err) {
      last = { ok: false, status: 0, outcome: "", error: String(err) };
    }
  }
  return last;
}

async function readOutcome(resp: Response): Promise<string> {
  try {
    const data = (await resp.json()) as { status?: string };
    return data?.status ?? "";
  } catch {
    return "";
  }
}

function jitter(ms: number): number {
  return Math.round(ms * (0.75 + Math.random() * 0.5));
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
