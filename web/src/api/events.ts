import { apiUrl, indexStatus } from "./client";
import type { IndexStatus } from "./types";

export type IndexTransport = "sse" | "poll" | "connecting";

export interface IndexStreamHandlers {
  onStatus: (s: IndexStatus) => void;
  onError?: (message: string) => void;
  onTransport?: (t: IndexTransport) => void;
}

const POLL_MS = 2000;

/**
 * SSE frames carry the bare status object per API.md; accept an enveloped
 * `{ok,data}` frame too so a bridge that reuses the JSON writer still works.
 */
function unwrapStatus(raw: unknown): IndexStatus | null {
  if (!raw || typeof raw !== "object") return null;
  const o = raw as Record<string, unknown>;
  if (o.ok === true && o.data && typeof o.data === "object") return o.data as IndexStatus;
  if (typeof o.state === "string") return o as unknown as IndexStatus;
  return null;
}

/** Two EventSource errors and we give up on SSE for this subscription. */
const SSE_ERROR_LIMIT = 2;

/** No open/message this long after subscribing ⇒ assume a buffering proxy. */
const CONNECT_TIMEOUT_MS = 6000;

/**
 * Live index status. Prefers SSE (`GET /index/events`); if EventSource errors
 * twice (or fails outright, or never delivers a frame — proxy.php buffering,
 * ancient nginx, daemon down) it permanently falls back to 2s polling of
 * `/index/status` for the life of the subscription.
 *
 * Returns an unsubscribe function.
 */
export function subscribeIndexStatus(h: IndexStreamHandlers): () => void {
  let closed = false;
  let es: EventSource | null = null;
  let pollTimer: ReturnType<typeof setInterval> | null = null;
  let connectTimer: ReturnType<typeof setTimeout> | null = null;
  let abort: AbortController | null = null;
  let errorCount = 0;

  const clearConnectTimer = () => {
    if (connectTimer) clearTimeout(connectTimer);
    connectTimer = null;
  };

  /** Close the stream for good and switch to polling. */
  function fallback(reason: string) {
    clearConnectTimer();
    es?.close();
    es = null;
    if (closed || pollTimer) return;
    h.onError?.(reason);
    startPolling();
  }

  const emitTransport = (t: IndexTransport) => {
    if (!closed) h.onTransport?.(t);
  };

  function startPolling() {
    if (closed || pollTimer) return;
    emitTransport("poll");
    const tick = async () => {
      if (closed) return;
      abort?.abort();
      abort = new AbortController();
      try {
        const s = await indexStatus(abort.signal);
        if (!closed) h.onStatus(s);
      } catch (e) {
        if (closed) return;
        if (e instanceof DOMException && e.name === "AbortError") return;
        h.onError?.(e instanceof Error ? e.message : "index status poll failed");
      }
    };
    void tick();
    pollTimer = setInterval(tick, POLL_MS);
  }

  function startSse() {
    if (closed) return;
    if (typeof EventSource === "undefined") {
      startPolling();
      return;
    }
    emitTransport("connecting");
    try {
      es = new EventSource(apiUrl("/index/events"));
    } catch {
      startPolling();
      return;
    }

    // A stream that never opens (proxy buffers the whole response) looks
    // healthy to EventSource, so time the handshake out ourselves.
    connectTimer = setTimeout(() => {
      fallback("live progress stream did not start — falling back to polling");
    }, CONNECT_TIMEOUT_MS);

    es.onopen = () => {
      clearConnectTimer();
      emitTransport("sse");
    };

    es.onmessage = (ev: MessageEvent<string>) => {
      if (closed) return;
      clearConnectTimer();
      errorCount = 0;
      emitTransport("sse");
      try {
        const s = unwrapStatus(JSON.parse(ev.data));
        if (s) h.onStatus(s);
      } catch {
        /* ignore keep-alive / non-JSON frames */
      }
    };

    es.onerror = () => {
      if (closed) return;
      errorCount += 1;
      // CLOSED means the browser gave up for good (non-2xx status, wrong
      // content-type, connection refused) — there is no self-reconnect to wait
      // for, so switch immediately instead of hanging on "connecting…".
      const dead = !es || es.readyState === EventSource.CLOSED;
      if (dead || errorCount >= SSE_ERROR_LIMIT) {
        fallback("live progress stream unavailable — falling back to polling");
      }
      // Otherwise EventSource is reconnecting on its own; give it one retry.
    };
  }

  startSse();

  return () => {
    closed = true;
    clearConnectTimer();
    es?.close();
    es = null;
    if (pollTimer) clearInterval(pollTimer);
    pollTimer = null;
    abort?.abort();
  };
}
