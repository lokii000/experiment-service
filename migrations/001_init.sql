-- Published configurations are append-only snapshots; a singleton pointer is
-- locked during a publish to serialize writers across Go instances.
CREATE TABLE IF NOT EXISTS config_snapshots (
    revision BIGINT PRIMARY KEY,
    document JSONB NOT NULL,
    published_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS config_current (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    revision BIGINT REFERENCES config_snapshots(revision)
);
INSERT INTO config_current(singleton,revision) VALUES (TRUE,NULL)
ON CONFLICT (singleton) DO NOTHING;

-- One accepted exposure per visitor/cohort; event_id dedupes network retries.
CREATE TABLE IF NOT EXISTS exposures (
    event_id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL,
    assignment_id TEXT NOT NULL,
    experiment_key TEXT NOT NULL,
    visitor_id TEXT NOT NULL,
    variant_key TEXT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(project_id, assignment_id, visitor_id)
);
CREATE INDEX IF NOT EXISTS exposures_cohort_variant
    ON exposures(project_id, assignment_id, variant_key);

-- Each conversion references the canonical accepted exposure.
CREATE TABLE IF NOT EXISTS conversions (
    event_id TEXT PRIMARY KEY,
    exposure_event_id TEXT NOT NULL REFERENCES exposures(event_id),
    goal TEXT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(exposure_event_id,goal)
);
CREATE INDEX IF NOT EXISTS conversions_exposure_goal
    ON conversions(exposure_event_id,goal);
