-- +goose Up
-- Origin, commit subject and repository from S3 history, the import run type, and the
-- Stack's git repository for commit links.
ALTER TABLE s3_history
    ADD COLUMN exec_kind  text NOT NULL DEFAULT '',
    ADD COLUMN exec_agent text NOT NULL DEFAULT '',
    ADD COLUMN message    text NOT NULL DEFAULT '',
    ADD COLUMN vcs_repo   text NOT NULL DEFAULT '';
ALTER TABLE s3_history DROP CONSTRAINT s3_history_type_check;
ALTER TABLE s3_history ADD CONSTRAINT s3_history_type_check
    CHECK (type IN ('up', 'refresh', 'destroy', 'import'));

ALTER TABLE runs
    ADD COLUMN exec_kind  text NOT NULL DEFAULT '',
    ADD COLUMN exec_agent text NOT NULL DEFAULT '',
    ADD COLUMN title      text NOT NULL DEFAULT '',
    ADD COLUMN vcs_repo   text NOT NULL DEFAULT '';
ALTER TABLE runs DROP CONSTRAINT runs_type_check;
ALTER TABLE runs ADD CONSTRAINT runs_type_check
    CHECK (type IN ('preview', 'up', 'refresh', 'destroy', 'import'));

ALTER TABLE stacks ADD COLUMN repo_url text NOT NULL DEFAULT '';

-- Re-read every history file once, so rows ingested before this change get the new fields
-- and the skipped resource-import files are ingested.
DELETE FROM s3_cursors;

-- +goose Down
ALTER TABLE stacks DROP COLUMN repo_url;
DELETE FROM runs WHERE type = 'import';
ALTER TABLE runs DROP CONSTRAINT runs_type_check;
ALTER TABLE runs ADD CONSTRAINT runs_type_check
    CHECK (type IN ('preview', 'up', 'refresh', 'destroy'));
ALTER TABLE runs DROP COLUMN vcs_repo, DROP COLUMN title, DROP COLUMN exec_agent,
    DROP COLUMN exec_kind;
DELETE FROM s3_history WHERE type = 'import';
ALTER TABLE s3_history DROP CONSTRAINT s3_history_type_check;
ALTER TABLE s3_history ADD CONSTRAINT s3_history_type_check
    CHECK (type IN ('up', 'refresh', 'destroy'));
ALTER TABLE s3_history DROP COLUMN vcs_repo, DROP COLUMN message, DROP COLUMN exec_agent,
    DROP COLUMN exec_kind;
