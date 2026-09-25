-- +goose Up
-- Engine log capture: '' = no log expected (imported, backfilled, not finished), 'pending' =
-- waiting for capture, 'captured' = run_changes has a source 'log' row, 'unavailable' = the
-- log was gone or unreadable.
ALTER TABLE runs ADD COLUMN log_status text NOT NULL DEFAULT ''
    CHECK (log_status IN ('', 'pending', 'captured', 'unavailable'));
CREATE INDEX runs_log_pending ON runs (ended_at) WHERE log_status = 'pending';

-- +goose Down
DROP INDEX runs_log_pending;
ALTER TABLE runs DROP COLUMN log_status;
