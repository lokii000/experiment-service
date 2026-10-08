import assert from 'node:assert/strict';
import { afterEach, test } from 'node:test';
import { postTrackingEvent, recordConversion, renderExperiment } from './browser-integration.js';

const originalFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = originalFetch; });

const sampleEvent = {
  event_id: '0fc4105e-7771-4b25-917d-a58f5b455172',
  project_key: 'pk_demo', experiment_key: 'checkout', assignment_id: 'cohort-1',
  visitor_id: 'visitor-3', variant_key: 'treatment', occurred_at: '2026-10-08T00:00:00Z',
};

test('retries 503 and preserves event id, timestamp, and entire payload', async () => {
  const bodies = [];
  globalThis.fetch = async (_, opts) => {
    bodies.push(opts.body);
    return new Response('{}', { status: bodies.length === 1 ? 503 : 201 });
  };
  const result = await postTrackingEvent({ endpoint: 'https://api.example', kind: 'exposure', event: sampleEvent, retryDelayMs: 0 });
  assert.equal(result.status, 201);
  assert.equal(bodies.length, 2);
  assert.deepEqual(bodies, [JSON.stringify(sampleEvent), JSON.stringify(sampleEvent)]);
});

test('respects 429 retry-after and succeeds', async () => {
  let calls = 0;
  globalThis.fetch = async () => {
    calls++;
    return calls === 1
      ? new Response('{}', { status: 429, headers: { 'Retry-After': '0' } })
      : new Response('{}', { status: 200 });
  };
  assert.equal((await postTrackingEvent({ endpoint: 'https://api.example', kind: 'exposure', event: sampleEvent, retryDelayMs: 0 })).status, 200);
  assert.equal(calls, 2);
});

test('does not retry bad requests', async () => {
  let calls = 0;
  globalThis.fetch = async () => { calls++; return new Response('{}', { status: 400 }); };
  const result = await postTrackingEvent({ endpoint: 'https://api.example', kind: 'exposure', event: sampleEvent, retryDelayMs: 0 });
  assert.equal(result.status, 400);
  assert.equal(calls, 1);
});

test('retries transient network errors but stops at limit', async () => {
  let calls = 0;
  globalThis.fetch = async () => { calls++; throw new Error('disconnected'); };
  await assert.rejects(postTrackingEvent({ endpoint: 'https://api.example', kind: 'exposure', event: sampleEvent, maxAttempts: 3, retryDelayMs: 0 }), /disconnected/);
  assert.equal(calls, 3);
});

test('assignment 429 immediately renders control and does not send exposure', async () => {
  let calls = 0;
  let control = 0;
  globalThis.fetch = async () => { calls++; return new Response('{}', { status: 429 }); };
  const result = await renderExperiment({
    endpoint: 'https://api.example', projectKey: 'pk_demo', experimentKey: 'checkout', visitorId: 'v',
    renderControl: () => { control++; }, renderVariant: () => assert.fail('should not render variant'),
  });
  assert.equal(control, 1);
  assert.equal(calls, 1);
  assert.equal(result.exposureDispatched, false);
});

test('conversion allows explicit id and retries with unchanged event data', async () => {
  const requests = [];
  globalThis.fetch = async (_, options) => {
    requests.push(JSON.parse(options.body));
    return new Response('{}', { status: requests.length === 1 ? 502 : 201 });
  };
  const response = await recordConversion({
    endpoint: 'https://api.example', projectKey: 'pk_demo', experimentKey: 'checkout',
    assignmentId: 'cohort-1', visitorId: 'v', goal: 'purchase', eventId: sampleEvent.event_id,
  });
  assert.equal(response.status, 201);
  assert.equal(requests.length, 2);
  assert.deepEqual(requests[0], requests[1]);
  assert.equal(requests[0].event_id, sampleEvent.event_id);
});
