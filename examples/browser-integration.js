/**
 * Minimal browser adapter: assignment has a short rendering deadline; tracking
 * retries in the background without blocking the page. The embedding site owns
 * stable visitor identity, consent, and actual-visible rendering callbacks.
 *
 * Delivery is best effort: a tab closing or browser offline for a long time can
 * still lose events. Production SDKs need consent-aware persistent queues.
 */

const transientStatus = new Set([429, 502, 503, 504]);

function retryDelay(response, attempt, initialDelayMs) {
  const backoff = Math.min(2000, initialDelayMs * (2 ** attempt));
  if (response?.status !== 429) return backoff;
  const rawRetryAfter = response.headers?.get?.("Retry-After");
  if (rawRetryAfter == null) return backoff;
  const seconds = Number(rawRetryAfter);
  return Number.isFinite(seconds) && seconds >= 0
    ? Math.min(2000, seconds * 1000)
    : backoff;
}

/**
 * Retry the *same serialized payload*, including event_id and occurred_at.
 * Returns the final HTTP Response. Nonretryable 4xx errors return immediately;
 * transport failures reject after the last attempt.
 *
 * Keepalive is best effort and doesn't guarantee delivery during navigation.
 */
export async function postTrackingEvent({
  endpoint,
  kind,
  event,
  maxAttempts = 3,
  retryDelayMs = 250,
  requestTimeoutMs = 1500,
}) {
  if (kind !== "exposure" && kind !== "conversion") {
    throw new Error("Unknown tracking event kind");
  }
  if (!Number.isInteger(maxAttempts) || maxAttempts < 1 || maxAttempts > 5) {
    throw new Error("maxAttempts must be between 1 and 5");
  }
  const body = JSON.stringify(event);
  const url = `${endpoint.replace(/\/$/, "")}/v1/events/${kind}`;
  let lastResponse;
  let lastError;

  for (let attempt = 0; attempt < maxAttempts; attempt++) {
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), requestTimeoutMs);
    try {
      const response = await fetch(url, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body,
        credentials: "omit",
        keepalive: true,
        signal: controller.signal,
      });
      if (response.ok || !transientStatus.has(response.status)) return response;
      lastResponse = response;
      lastError = undefined;
    } catch (err) {
      lastError = err;
      lastResponse = undefined;
    } finally {
      clearTimeout(timeout);
    }

    if (attempt + 1 < maxAttempts) {
      await new Promise((resolve) => setTimeout(resolve, retryDelay(lastResponse, attempt, retryDelayMs)));
    }
  }
  if (lastResponse) return lastResponse;
  throw lastError;
}

export async function renderExperiment({
  endpoint,
  projectKey,
  experimentKey,
  visitorId,
  renderControl,
  renderVariant,
  onExposure,
  deadlineMs = 100,
}) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), deadlineMs);
  let decision;
  try {
    const response = await fetch(`${endpoint.replace(/\/$/, "")}/v1/assignments`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        project_key: projectKey,
        visitor_id: visitorId,
        experiment_keys: [experimentKey],
      }),
      signal: controller.signal,
      credentials: "omit",
    });
    // Includes 429: fail safe to control instead of waiting on rendering.
    if (!response.ok) throw new Error("experiment API unavailable");
    decision = (await response.json()).assignments?.[0];
  } catch (_) {
    renderControl();
    return { rendered: "default", exposureDispatched: false };
  } finally {
    clearTimeout(timer);
  }

  if (decision?.status !== "assigned") {
    renderControl();
    return { rendered: "default", exposureDispatched: false };
  }
  try {
    if (decision.variant_key === "control") renderControl();
    else renderVariant(decision.variant_key);
  } catch (_) {
    try { renderControl(); } catch (_) { /* host fallback owns errors */ }
    return { rendered: "default", exposureDispatched: false };
  }

  // If rendering is asynchronous, move tracking to the renderer's actual
  // visible callback. Assignment alone is NOT an exposure.
  const event = {
    event_id: crypto.randomUUID(),
    project_key: projectKey,
    experiment_key: experimentKey,
    assignment_id: decision.assignment_id,
    visitor_id: visitorId,
    variant_key: decision.variant_key,
    occurred_at: new Date().toISOString(),
  };
  try {
    // Custom onExposure hooks own their own delivery/retry policy.
    const sending = onExposure
      ? onExposure(event)
      : postTrackingEvent({ endpoint, kind: "exposure", event });
    void Promise.resolve(sending).catch(() => {});
  } catch (_) { /* tracking failure must never break customer rendering */ }
  return {
    rendered: decision.variant_key,
    exposureDispatched: true, // dispatched, NOT necessarily persisted
    assignmentId: decision.assignment_id,
  };
}

/**
 * Call only when a real conversion occurs, after its exposure is persisted.
 * Returns the final HTTP response; callers should check response.ok.
 * For 409 exposure_not_found, wait for the exposure to commit before retrying
 * with the same eventId. A new invocation generates a new ID unless supplied.
 */
export function recordConversion({
  endpoint, projectKey, experimentKey, assignmentId, visitorId, goal,
  eventId = crypto.randomUUID(),
}) {
  return postTrackingEvent({
    endpoint,
    kind: "conversion",
    event: {
      event_id: eventId,
      project_key: projectKey,
      experiment_key: experimentKey,
      assignment_id: assignmentId,
      visitor_id: visitorId,
      goal,
      occurred_at: new Date().toISOString(),
    },
  });
}
