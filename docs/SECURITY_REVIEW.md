# Security and code review — 2026-10-08

Scope: reviewed `cmd/api`, `internal/httpapi`, `internal/config`, `internal/assignment`, `internal/storage`, `internal/tracking`, `examples/browser-integration.js`, Docker Compose, Dockerfile, `.env.example` and SQL migration. This is a targeted source review, **not a penetration test** or a formal production security certification.

## Release-blocking decisions

1. **Public API abuse (HIGH, PARTIALLY MITIGATED).** Public routes intentionally do not authenticate visitors; CORS is not authentication. In-process per-TCP-peer and global limits now cover assignments, shared tracking routes, and admin APIs; exceeded limits return HTTP 429 and `Retry-After`. These controls do not provide distributed tenant/project quotas, correct client-IP partitioning behind an untrusted proxy, or complete protection from invented traffic. Add hosting-edge limits, project quotas, tracking backpressure and 429/ingestion-rate observability before production use.
2. **Fabricated event identity (HIGH, ACCEPTED MVP LIMITATION).** Clients control visitor IDs and timestamps, and can invent plausible exposure/conversion events by requesting assignments first. Existing server-side variant verification is necessary but not identity authentication. Signed assignment receipts, abuse monitoring, visitor-identity policy, and bot controls are production follow-ups. Discuss this explicitly in `DESIGN.md`.
3. **Deployment accidentally starts without DB (HIGH, PATCHED).** DB mode was chosen whenever `DATABASE_URL` was non-empty, otherwise API silently entered assignment-only mode. The new `REQUIRE_DATABASE=true` flag makes missing DB configuration fail startup; Compose sets it by default. Local file mode remains available when the flag is unset.
4. **Unsupported runtime images (VERIFIED FIX).** Builder was `golang:1.23-alpine`, runtime `alpine:3.20`. The local Docker build and restart now use `golang:1.27.1-alpine3.24` with `alpine:3.24`. Rebuild and consider digest-pinning for the actual hosted release.
5. **Build context could include `.env` (PATCHED).** Missing `.dockerignore` allowed local secret files to enter the Docker build context, even though not copied into final image. `.dockerignore` now excludes secrets, local binaries, VCS metadata, and unnecessary files.

## Medium-priority findings

- **CORS parsing (PATCHED):** `strings.Split(value, ", ")` mishandled comma-separated origins without spaces. Split on comma; the existing HTTP server trims each origin. Added a regression test. CORS is only browser-side access control.
- **Browser transient retries (PATCHED, RESIDUAL LIMIT):** `examples/browser-integration.js` retries network/timeout errors and HTTP 429/502/503/504 for up to three attempts with an unchanged event ID and serialized payload; six Node tests passed. Rendering remains nonblocking. The browser still lacks a durable offline queue, and conversion `409 exposure_not_found` requires an application-managed retry.
- **Readiness semantics (OPEN):** `/readyz` checks only that an assignment snapshot exists, so it remains `200` during DB outages. Document assignment-only readiness and expose a separate private tracking-ready check if the deployment needs it.
- **Unbounded stale serving after DB failure (OPEN):** config refresh keeps the last snapshot indefinitely, including potentially stale pause/kill-switch decisions. Explicitly define max-staleness and fail-safe behavior versus assignment availability; propagate emergency stops separately.
- **PII/retention (OPEN):** raw opaque visitor IDs are stored in PostgreSQL; no automated retention, project purge/deletion, or privacy policy is implemented. Define retention duration and deletion tooling before serving real customer data.

## Additional code-quality observations

- The app duplicates schema DDL in `internal/storage/config_repository.go` and `migrations/001_init.sql`; one migration authority is preferable. Running DDL automatically during startup also requires broad database privileges.
- Admin authentication is a single shared environment-variable bearer token. Adequate for a demonstrator behind HTTPS, but missing tenant-scoped roles, token rotation, publication audit actor identity and centralized edge-rate limiting. In-process admin throttling is now implemented.
- Inputs are bounded in bytes and unknown JSON fields are rejected, but identifier character and event-ID format validation are permissive. Restrict event IDs to UUID and key fields to a documented safe character set if the SDK/API contract requires it.
- No structured request-ID propagation, per-path latency metrics, 429 counters, or config-refresh age gauge. Add the observability signals that justify production-scale claims.
- `tracking.Service` uses client-provided `occurred_at` to order exposure/conversion events. Browser clocks are untrusted; note the seven-day window and skew limitations in the design.
- No secret values were written into this review. Ensure `.env` remains excluded from source control, use strong production credentials, and require HTTPS/TLS from browser to edge and app to managed PostgreSQL.

## Verified strengths

- DB calls use parameterized SQL placeholders rather than string concatenation.
- Admin bearer token SHA-256 digests are compared with `crypto/subtle.ConstantTimeCompare`.
- HTTP JSON bodies use a 64-KiB ceiling, unknown-field rejection, and trailing-JSON rejection.
- HTTP, database and shutdown paths have bounded timeouts.
- PostgreSQL unique keys and conflict queries provide deduplication by event ID and by visitor/cohort/goal.
- Control-plane publication is transactional with revision precondition and row lock; assignment hot path reads immutable compiled snapshots.

## Validation status and remaining deployment gate

```bash
go test ./... -count=1
go test -tags pgx ./... -count=1
go test -race -tags pgx ./... -count=1
go vet -tags pgx ./...
docker compose config --quiet
docker compose build --pull
```

Local verification: Go/race/static checks passed, browser retries passed all six Node tests, new rate-limit tests passed, and the Go/Alpine Docker image was rebuilt on the developer's Mac with PostgreSQL mode. A dedicated PostgreSQL 16 integration test was run and passed (including as part of the full Go suite with `TEST_DATABASE_URL` set); a later run without that variable correctly skipped the test.

**Hosted verification (2026-10-08):** Railway deployed the API with `database_mode=true`; public HTTPS health and readiness checks returned 200, unauthenticated admin results returned 401, multi-experiment assignment returned 200, and a synthetic exposure (201), replay (200 duplicate), conversion (201) were reflected in the authenticated results (treatment 1/1, control 0/0). Live URL: https://api-production-1d91.up.railway.app. **Still unverified:** managed PostgreSQL TLS requirements, secret rotation, trusted-proxy/edge quota enforcement, multi-replica behavior, observability and privacy/retention. Do not treat a passing smoke test as a production security audit.

**Release decision:** Core logic is good for the take-home. Public deployment should be gated on supported base images, protected secrets, authenticated admin access over TLS, basic traffic limits, and a documented residual-risk statement for forged events and best-effort browser tracking.
