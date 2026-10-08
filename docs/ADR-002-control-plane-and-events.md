# ADR-002: Database-backed configuration, idempotent tracking and reporting

**Status:** Implemented. Real PostgreSQL integration passed locally. A Railway HTTPS deployment and synthetic end-to-end persistence smoke test passed on 2026-10-08; production hardening remains in scope for later work.

## Context

Serving assignments must stay independent of remote storage during the render-critical request. Configuration changes must nevertheless survive restarts and concurrent publishers, while tracking events need durable idempotency and trustworthy attribution.

## Decisions

1. Keep the Go modular monolith. PostgreSQL provides the control-plane source of truth and event storage; requests on the warm assignment path never perform database IO.
2. Publish complete JSONB configuration snapshots using a single `config_current` pointer, a transaction-level row lock, and an optimistic `expected_revision`. Preserve append-only historical snapshots for audits.
3. Validate the entire proposed configuration and compare it with the previously published snapshot **inside the publisher transaction**. Reject changing immutable assignment identity, variant weights/order, decreasing enrollment allocation, or removing previously published experiments.
4. Refresh validated in-memory snapshots from PostgreSQL every two seconds. Serve last-known-good during temporary DB outages. Pause publication is eventually consistent; strong kill-switch propagation is a future separate mechanism.
5. Track first unique exposure per `(project_id, assignment_id, visitor_id)` and first conversion per `(exposure_event_id, goal)`. `event_id` uniqueness also handles exact network retries.
6. Reject event-ID reuse with contradictory data. A semantically duplicate event with a fresh ID counts only once.
7. Verify exposure variant against the deterministic allocation before persistence. Reference the persisted exposure during conversion attribution. This is correctness validation, not strong proof of browser behavior.
8. In the MVP, conversions that arrive before exposure commit return `409 exposure_not_found`; the SDK/client must retry with the same event ID. Avoid falsely claiming exactly-once delivery.
9. Derive descriptive results from separate SQL aggregates, scoped by immutable cohort and goal. Expose zero-exposure variants as `null` conversion rate and make no statistical winner claim.
10. Use an admin bearer token for control-plane and reporting routes. Public project keys are identifiers only and not secrets.
11. Apply per-TCP-peer and per-instance in-memory token-bucket throttles to assignment, shared tracking routes, and admin APIs; return HTTP 429 plus `Retry-After` on rejection.
12. The example browser adapter uses a short assignment deadline with control fallback and retries transient tracking failures (network/timeout, 429, 502, 503, 504) with unchanged serialized event data. This is best-effort, not guaranteed event delivery.

## Consequences and limitations

- A global snapshot simplifies transactional publication but whole-document updates are inefficient for very large multi-tenant deployments. Normalize by project/experiment later with transactional publishing and explicit immutable cohorts.
- In-process caches reduce latency but can briefly disagree on status and ramp-up enrollment during propagation; a startup that cannot reach the DB in database mode fails rather than serving an unknown configuration.
- Semantic uniqueness and Postgres transactions protect storage, not browser-to-server delivery. Network failures, missing consent, fabricated traffic, and delayed events can bias metrics.
- Browser-side rendering/flicker policies and identity correctness remain customer integration responsibilities; the included adapter is minimal. Its transient retries do not survive browser closure and do not automatically resolve conversion-before-exposure 409 errors.
- Per-instance rate limits use TCP peer identity and can group unrelated clients behind a proxy; enforce trusted edge-level quotas in deployment.
- Reporting queries are efficient for a small deployment, but a billion-row event store requires read models, event retention/partitioning, and an independently scalable data pipeline.
- Live PostgreSQL integration tests passed on a dedicated local PostgreSQL database; Docker build, local smoke and failure-mode tests also passed. Railway external HTTPS assignment → exposure → conversion → results smoke test passed. Multi-replica tests, database TLS/secret/edge-quota verification, and full production readiness reviews remain pending.

## Evolution triggers

- Measured write IOPS/WAL/index growth: add durable event stream with idempotent consumers, then dedicated aggregates.
- Growing tenant counts: per-project configuration publishing and RBAC rather than global JSONB replacement.
- Strong pause SLA: distributed invalidation or explicit kill switch, not only polling.
- Higher attribution completeness: durable inbox for out-of-order events, explicit attribution windows and reconciliation.
- Reporting rigor: SRM checks, maturity-aware cohorts, multiple-comparison/sequential statistical testing, and bot filtering.
