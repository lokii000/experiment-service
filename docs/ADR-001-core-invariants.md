# ADR-001: Immutable experiment cohorts and stateless assignment

**Status:** Accepted; assignment invariants implemented, PostgreSQL publishing described in ADR-002
**Context:** Assignment is on the customer page-rendering critical path, but configuration and analytics evolve independently.

## Decision

1. Use a single Go deployable with separate packages for serving and configuration.
2. No visitor-assignment persistence: variant selection is a deterministic function of visitor ID, immutable cohort identity, project ID, algorithm version and canonical allocation.
3. Split enrollment and variant decisions via purpose-separated SHA-256 hashes over length-prefixed inputs.
4. Use integer basis points and precomputed variant ranges; never use floating point to select a variant.
5. Treat variant keys, order, and weights as immutable for a published cohort. A weight change creates a new cohort/assignment identity. Administrative `revision` never enters hashing.
6. Permit only monotonic traffic *increases* for an existing cohort; reject decreases. Pausing returns `inactive` rather than a new variant.
7. Publish full validated configuration snapshots atomically in process. PostgreSQL mode additionally persists snapshots under a transactional compare-and-swap publish; cross-instance updates converge by polling rather than synchronous consensus.
8. Do not block the customer website when assignment or tracking fails. A browser deadline and normal/default rendering are the external safety guarantee.
9. Do not acknowledge tracking writes before durable acceptance; database-mode tracking uses unique constraints and exposure-linked attribution.
10. Report descriptive conversions and rates only until statistical tests and quality checks are explicitly designed.

## Consequences

- Stateless serving and cheap horizontal reads, but immutable allocation limits live reweighting.
- An increase in traffic is monotonic, but cached stale traffic values can temporarily exclude new visitors; existing enrolled assignments remain stable.
- Pause propagation may be delayed, especially while serving stale config during a storage outage.
- A configuration revision does not split statistical cohorts; a new cohort identity does.
- File-only startup validates inputs but does **not** provide durable publication. PostgreSQL mode persists authoritative snapshots, but cross-replica status propagation remains eventually consistent.

## Alternatives rejected for MVP

- SQL read/write per assignment (critical-path dependency, unbounded assignment table).
- Mutable 50/50 to 90/10 weights under the same cohort (would reassign existing visitors).
- Treating `revision` as part of the hash (rebuckets on harmless administrative updates).
- Automatic full microservices/Kafka/Redis deployment (unjustified by exercise scale).

## Questions to revisit

- Does the product require sticky variant membership across a new cohort? If so, a stored/SDK-carried assignment mechanism and migration rules become necessary.
- How fast must an emergency kill switch propagate during config-backend outage?
- How will cross-device identities, shared devices, and experiment interference be handled?
- What durable reconciliation policy is required for out-of-order and late events?
