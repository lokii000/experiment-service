# Experiment Assignment Service

A small **Go + PostgreSQL** experimentation backend for deterministic, sticky A/B assignments; safe experiment configuration; durable, deduplicated exposure/conversion events; and descriptive experiment results. Includes a minimal browser integration with a fail-safe rendering deadline and bounded event retries.

**Project status (2026-10-08):** implemented, locally tested, and deployed on Railway with PostgreSQL-backed mode. Public HTTPS health, assignment, unauthorized-admin, exposure, duplicate exposure, conversion, and authenticated results smoke tests passed. This is an engineering take-home demonstrator, not a production-certified experimentation platform.

## Live deployment (Railway)

**Public API base URL:** [https://api-production-1d91.up.railway.app](https://api-production-1d91.up.railway.app)

This service exposes JSON APIs, **not a graphical UI**. Try the health endpoint: [https://api-production-1d91.up.railway.app/healthz](https://api-production-1d91.up.railway.app/healthz).

```bash
curl -fsS https://api-production-1d91.up.railway.app/healthz
curl -fsS https://api-production-1d91.up.railway.app/readyz
curl -fsS -X POST https://api-production-1d91.up.railway.app/v1/assignments \
  -H 'Content-Type: application/json' \
  -d '{"project_key":"pk_demo","visitor_id":"readme-demo-visitor","experiment_keys":["checkout_button_v1","homepage_layout_v1"]}'
```

On **2026-10-08**, external HTTPS checks returned `200` for health/readiness, `401` for admin results without a token, and `200` for multi-experiment assignment. A unique synthetic visitor produced `201 recorded` exposure, `200 duplicate` for a replay, and `201 recorded` conversion. Authenticated SQL-backed results returned one treatment exposure and one conversion; zero-exposure control had `conversion_rate: null`. **These are synthetic smoke-test events, not statistical evidence of a winning variant.** The admin token is never exposed in this README.

Railway uses runtime environment variables for `DATABASE_URL`, `ADMIN_TOKEN`, `REQUIRE_DATABASE=true`, and CORS settings. The public edge uses HTTPS. Database TLS enforcement, edge quota configuration, secret rotation, log/metrics coverage and production-grade privacy safeguards require an independent deployment review. See [Security Review](docs/SECURITY_REVIEW.md).

## Architecture in 60 seconds

```text
Browser / embedding site
  ├─ POST /v1/assignments ───────────▶ Go API ──▶ immutable in-memory config
  └─ POST /v1/events/{exposure,conversion} ─────▶ PostgreSQL
Administrator
  ├─ GET/POST /v1/admin/config ─────▶ PostgreSQL snapshots + active revision
  └─ GET /v1/admin/results ────────▶ PostgreSQL aggregates

Go API periodically refreshes its cached configuration from PostgreSQL.
```

- **No per-visitor assignment storage:** deterministic SHA-256 bucketing, 10,000 integer buckets, independent enrollment and variant selection.
- **Sticky cohorts:** changing a revision does not rebucket; already-published allocation/variant definitions cannot be mutated in place.
- **Durable events:** an exposure is counted once per visitor/cohort, a conversion once per linked exposure/goal, with both event-ID and semantic deduplication.
- **Page-render safety:** browser assignment defaults to a 100 ms deadline and renders control on failure; background tracking retries transient errors with an unchanged event ID.
- **Operational scope:** one Go deployment, PostgreSQL, no Redis/Kafka; in-process limits are per peer and per instance.

Full reasoning: [System design](docs/DESIGN.md) · [HLD, LLD and validation](docs/ARCHITECTURE_AND_VALIDATION.md) · [Core invariants](docs/ADR-001-core-invariants.md) · [Persistence ADR](docs/ADR-002-control-plane-and-events.md) · [Security review](docs/SECURITY_REVIEW.md).

## 1. Run locally (recommended: Docker Compose)

**Requirements:** Docker Desktop / Docker Engine with Compose, and network access for pulling images. The current Dockerfile uses Go `1.27.1-alpine3.24` and runtime `alpine:3.24`; the application module targets Go 1.23 language compatibility.

From the repository root:

```bash
cp .env.example .env
```

Edit `.env` and set **unique values** for `POSTGRES_PASSWORD` and `ADMIN_TOKEN` (at least 16 characters). Keep it out of version control; `.gitignore` and `.dockerignore` exclude local secrets. If using an already-initialized Docker volume, changing `POSTGRES_PASSWORD` alone does **not** change that database user's existing password; preserve the working local credentials or rotate them in PostgreSQL intentionally.

```bash
docker compose config --quiet
docker compose up -d --build
docker compose ps
docker compose logs --tail=30 api
curl -fsS http://localhost:8080/healthz
curl -fsS http://localhost:8080/readyz
```

On first boot, `config/experiments.json` seeds the database. Later restarts load the **published PostgreSQL configuration** instead of replacing it with the seed. Data is stored in a Compose volume. **Do not run `docker compose down -v` unless you intend to delete local data.**

Local Compose uses `sslmode=disable` for container-to-container PostgreSQL traffic, not as an acceptable setting for a publicly hosted database. The Compose setup sets `REQUIRE_DATABASE=true` to fail fast on missing database configuration.

### Direct Go development

With Go 1.23+:

```bash
go run ./cmd/api
```

This is **assignment-only file mode** (no durable tracking, reporting, or publishing). To run PostgreSQL mode, provide a reachable DSN and admin token, and build with the `pgx` tag:

```bash
export DATABASE_URL='postgres://experiment:YOUR_LOCAL_PASSWORD@127.0.0.1:5432/experiments?sslmode=disable'
export ADMIN_TOKEN='YOUR_LONG_LOCAL_ADMIN_TOKEN'
export REQUIRE_DATABASE=true
export CONFIG_PATH=config/experiments.json
go run -tags pgx ./cmd/api
```

Do not use these example credentials unchanged or place secrets in repository files. Database mode currently runs its schema initialization on startup; a separate migration job/limited runtime DB role is the preferred production model.

## 2. Demo configuration and assignment API

Seeded `project_key`: `pk_demo`.

| Experiment | Traffic | Variants |
|---|---:|---|
| `checkout_button_v1` | 80% enrolled | `control` 50%, `treatment` 50% |
| `homepage_layout_v1` | 100% enrolled | `control` 50%, `compact` 30%, `expanded` 20% |

**Multiple experiments in one request:**

```bash
curl -fsS -X POST http://localhost:8080/v1/assignments \
  -H 'Content-Type: application/json' \
  -d '{"project_key":"pk_demo","visitor_id":"visitor-123","experiment_keys":["checkout_button_v1","homepage_layout_v1"]}'
```

A result contains an `assignments` array. Each entry has `experiment_key` and `status`, one of `assigned`, `not_enrolled`, `inactive`, or `unknown_experiment`. Assigned entries also contain `variant_key` and immutable cohort `assignment_id`. **Do not log an exposure just because an assignment was returned; do so only after that variant was rendered.**

## 3. Event and admin APIs

| Method/path | Access | Description |
|---|---|---|
| `POST /v1/assignments` | Public project key | Deterministic multi-experiment decisions |
| `POST /v1/events/exposure` | Public project key | Record a rendered assignment once per visitor/cohort |
| `POST /v1/events/conversion` | Public project key | Attribute a goal once to an existing exposure |
| `GET /v1/admin/config` | Admin bearer token | Load published configuration and revision |
| `POST /v1/admin/config` | Admin bearer token | CAS-publish a complete config document |
| `GET /v1/admin/results` | Admin bearer token | Exposure, conversion, and rate per variant/goal |
| `GET /healthz` | Unauthenticated | Process liveness |
| `GET /readyz` | Unauthenticated | **Assignment-snapshot readiness**, not DB write readiness |

### Exposure example

Use the `assignment_id` and **actual** `variant_key` received from `POST /v1/assignments`. Generate a fresh unique `event_id` and current UTC `occurred_at`:

```json
{
  "event_id": "274b4ee7-e2f2-4a08-9846-cd8ae4c0cdb2",
  "project_key": "pk_demo",
  "experiment_key": "checkout_button_v1",
  "assignment_id": "cohort-checkout-button-001",
  "visitor_id": "visitor-123",
  "variant_key": "control",
  "occurred_at": "2026-10-08T09:00:00Z"
}
```

Send to `POST /v1/events/exposure`. The example variant and timestamp are illustrative, **not values to submit unchanged**. An accepted event returns `201 {"status":"recorded"}`; a replay returns `200 {"status":"duplicate"}`. Conflicting data under an existing event ID returns `409`. The server recomputes the canonical variant before persistence.

### Conversion example

```json
{
  "event_id": "323a5d6c-a791-44a0-8c2b-3caaf5b72791",
  "project_key": "pk_demo",
  "experiment_key": "checkout_button_v1",
  "assignment_id": "cohort-checkout-button-001",
  "visitor_id": "visitor-123",
  "goal": "checkout_completed",
  "occurred_at": "2026-10-08T09:01:00Z"
}
```

Send to `POST /v1/events/conversion` **after** that visitor's exposure has been persisted. A `409 {"error":"exposure_not_found"}` means the exposure has not committed yet. Retry after exposure persistence with the same event ID, rather than claiming a conversion. Conversion timestamps cannot precede the linked exposure.

### Admin reads and publishing

The administrative token must remain server-side; never include it in browser JavaScript or a public repository.

```bash
curl -sS http://localhost:8080/v1/admin/config \
  -H "Authorization: Bearer ${ADMIN_TOKEN}"

curl -sS 'http://localhost:8080/v1/admin/results?project_key=pk_demo&experiment_key=checkout_button_v1&goal=checkout_completed' \
  -H "Authorization: Bearer ${ADMIN_TOKEN}"
```

For a configuration update, `POST /v1/admin/config` accepts `{"expected_revision": <current global revision>, "configuration": <complete config document>}`. First obtain `revision` and `configuration` with the GET endpoint; update the **complete** document, increment the affected experiment's revision for status/traffic changes, and publish it with the prior global `expected_revision`.

```bash
curl -i -X POST http://localhost:8080/v1/admin/config \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" \
  -H 'Content-Type: application/json' \
  --data-binary @publish.json
```

A stale expected revision returns 409. Published variants/weights/order cannot be changed in place, traffic allocation cannot decrease, and published experiments cannot be removed. Use a new cohort to change an allocation.

Results return all variants, including zero-exposure variants with `conversion_rate: null`. Rates are **descriptive only**; no statistical winner is claimed.

## 4. Browser integration and retries

See [`examples/browser-integration.js`](examples/browser-integration.js):

- `renderExperiment({ endpoint, projectKey, experimentKey, visitorId, renderControl, renderVariant, ... })` fails safe to control after the default **100 ms** assignment deadline.
- Tracking is emitted **after synchronous rendering** and never awaited in the rendering path. For asynchronous or deferred rendering, track from the actual visibility callback.
- The default event transport retries network/timeouts and HTTP **429, 502, 503, 504**, up to **3 total attempts**, using an **identical event ID and serialized body** on every attempt. HTTP 429 `Retry-After` is respected with a bounded delay.
- `recordConversion(...)` resolves to the final HTTP response; the caller checks `.ok`. Automatic retries do not resolve `409 exposure_not_found` because that requires ordering against a committed exposure.
- Host site must provide stable visitor identity and honor consent; events are **best-effort** and can be lost when a tab closes. No local durable queue is implemented.

For a real integration, import the module into your bundler and call it during the rendering lifecycle; the example is not a drop-in vendor-hosted SDK.

## 5. Rate limits and security defaults

The API implements per-peer **in-memory token buckets** plus a per-instance aggregate ceiling (10× per-peer values). Limits for exposure and conversion share the same tracking budget.

| API group | Default peer RPS | Burst | Environment variables |
|---|---:|---:|---|
| Assignment | 1,500 | 3,000 | `ASSIGN_RATE_RPS`, `ASSIGN_RATE_BURST` |
| Tracking | 60 | 120 | `TRACK_RATE_RPS`, `TRACK_RATE_BURST` |
| Admin | 10 | 20 | `ADMIN_RATE_RPS`, `ADMIN_RATE_BURST` |

Limited requests return **HTTP 429**, `Retry-After: 1`, and `{"error":"rate_limited"}`. Values must be positive integers. Limits are scoped to the **TCP peer**, not an unverified forwarding header. Behind a reverse proxy, all callers may share the proxy address; the buckets are **not distributed across replicas**. Add trusted edge-level controls and tenant/project quotas before serving untrusted internet traffic.

Additional controls include allowlisted CORS origins, 64 KiB JSON body limit, rejection of unknown/trailing JSON fields, PostgreSQL parameterization, admin bearer-token verification, non-root Docker runtime, and database-required deployment mode. CORS and a public project key are **not** authorization. Clients can still fabricate visitor IDs/events, and a privacy/retention policy is not implemented. See [Security Review](docs/SECURITY_REVIEW.md).

## 6. Tests and verified local evidence

### Unit, race, static analysis and browser tests

```bash
go test -tags pgx ./... -count=1 -v
go test -race -tags pgx ./... -count=1
go vet -tags pgx ./...
node --test examples/browser-integration.test.mjs
```

Node **22+** is recommended for the JavaScript tests. The full test suite was executed locally with no Go test failures, no race detector findings, and no `go vet` output. All **six browser retry tests** passed, as did the new rate-limiter test cases.

### Run **real** PostgreSQL integration tests (separate expendable DB)

The storage integration test **skips** when `TEST_DATABASE_URL` is unset. To make sure the database test actually runs, use a dedicated disposable PostgreSQL container; do not point tests at your application database.

```bash
docker run -d \
  --name experiment-postgres-test \
  -e POSTGRES_USER=experiment \
  -e POSTGRES_PASSWORD=integration_test_password_123 \
  -e POSTGRES_DB=experiments_test \
  -p 127.0.0.1:5433:5432 \
  postgres:16-alpine

docker exec experiment-postgres-test pg_isready -U experiment -d experiments_test

export TEST_DATABASE_URL='postgres://experiment:integration_test_password_123@127.0.0.1:5433/experiments_test?sslmode=disable'
go test -tags pgx ./internal/storage -run Integration -count=1 -v
go test -tags pgx ./... -count=1
unset TEST_DATABASE_URL

docker stop experiment-postgres-test
```

If the named test container already exists but is stopped, use `docker start experiment-postgres-test` **instead of** `docker run`. The test creates a temporary schema and drops it afterward. These static credentials are for a disposable **localhost-only test DB**, never a hosted service.

**Verified:** `TestIntegrationConfigPublishingAndEventCounting` has passed both by itself and as part of the complete DB-enabled Go suite on the developer's Mac. In a later run without `TEST_DATABASE_URL`, that test correctly reported `SKIP`, not failure.

### Manual resilience tests and local throughput

On a running local Compose setup, verified behavior included:

- Assignment remains responsive while PostgreSQL is stopped (last-known-good snapshot).
- A valid exposure returns **503** during DB outage; retry after recovery yields **201 recorded** and then **200 duplicate** on replay.
- Twenty concurrent same-visitor exposure submissions resulted in **one 201** and **nineteen 200 duplicates**, without request failures.
- Three 30-second local assignment tests (two experiments per request) completed **47,980 successful requests**, zero reported failures/drops. Achieved **100.0 / 499.5 / 999.7 RPS** and p95 **1.69 / 1.42 / 0.99 ms** at the three test settings.

These are **local results measured before the latest rate-limit change**, not internet-facing production benchmarks or guarantees. See the detailed methodology and limitations in [DESIGN.md](docs/DESIGN.md).

## 7. Operational behavior and deployment notes

- `GET /healthz` is liveness. `GET /readyz` asserts an assignment snapshot exists; it **does not** check PostgreSQL write health. Monitor tracking errors independently.
- HTTP server uses bounded read/write timeouts; database handlers use context deadlines. Cached config refreshes every **2 seconds**. A prolonged DB outage can keep cached pause/traffic settings stale; there is no independent emergency kill switch.
- Local Compose writes PostgreSQL data to a named volume; an application/API restart does not erase events or published configurations.
- The Railway HTTPS deployment has passed external API and persistence smoke tests. Before serving real traffic, verify managed PostgreSQL TLS, secrets rotation, edge throttling/trusted proxy handling, monitoring, retention and tenant controls. The current demonstration does not certify those controls.
- The application currently performs table initialization at startup and keeps migrations in `migrations/001_init.sql`. A production migration pipeline should own schema changes separately; the application should not retain DDL privileges long-term.
- Public event APIs accept client IDs and timestamps, so deduplication does **not** imply identity verification. No deletion/retention or tenant-admin RBAC has been added yet.

### Future evolution (not required for this exercise)

Introduce project-scoped config publication, distributed/edge serving and explicit kill-switch propagation when measured business requirements justify them. For very large event volume, add durable ingestion, idempotent stream consumers, partitioned event retention and precomputed reporting. Statistical analysis requires SRM, power/confidence definitions and sequential-testing controls before making experiment decisions.

## 8. Repository structure

```text
cmd/api/                     API entrypoint, build-tagged pgx driver
internal/assignment/         deterministic bucketing + invariants
internal/config/             validated immutable snapshots
internal/httpapi/            public/admin routes + rate limiter
internal/tracking/           validation and attribution service
internal/storage/            PostgreSQL persistence, reporting and integration tests
config/experiments.json      local seed configuration
migrations/001_init.sql      schema reference
examples/browser-integration.js
examples/browser-integration.test.mjs
loadtest/assignment.go       local assignment load generator
docs/DESIGN.md               system design, trade-offs, evidence
docs/ADR-001-core-invariants.md
docs/ADR-002-control-plane-and-events.md
docs/SECURITY_REVIEW.md
Dockerfile
docker-compose.yml
.env.example
```

**Submission status:** the Railway public HTTPS smoke test passed on 2026-10-08, and the source is published on GitHub. This is a verified take-home demonstration, not a production readiness or security certification.
