-- +goose Up
ALTER TABLE stacks
    ADD COLUMN backend_url   text NOT NULL DEFAULT '',
    ADD COLUMN project       text NOT NULL DEFAULT '',
    ADD COLUMN pulumi_stack  text NOT NULL DEFAULT '',
    ADD COLUMN s3_error      text NOT NULL DEFAULT '',
    ADD COLUMN s3_checked_at timestamptz;

ALTER TABLE runs DROP CONSTRAINT runs_commit_source_check;
ALTER TABLE runs ADD CONSTRAINT runs_commit_source_check
    CHECK (commit_source IN ('', 'update', 'stack', 'history'));

CREATE TABLE s3_history (
    key        text        PRIMARY KEY,
    bucket     text        NOT NULL,
    namespace  text        NOT NULL,
    stack_name text        NOT NULL,
    type       text        NOT NULL CHECK (type IN ('up', 'refresh', 'destroy')),
    state      text        NOT NULL CHECK (state IN ('succeeded', 'failed')),
    started_at timestamptz NOT NULL,
    ended_at   timestamptz NOT NULL,
    commit     text        NOT NULL DEFAULT '',
    counts     jsonb       NOT NULL DEFAULT '{}',
    seen_at    timestamptz NOT NULL,
    link_state text        NOT NULL DEFAULT 'pending'
        CHECK (link_state IN ('pending', 'linked', 'imported', 'ambiguous')),
    run_id     bigint      REFERENCES runs (id) ON DELETE SET NULL
);
CREATE INDEX s3_history_pending ON s3_history (ended_at) WHERE link_state = 'pending';
CREATE INDEX s3_history_prefix ON s3_history (bucket, key);

-- The listing position per history prefix. It moves past every processed key, including
-- ones skipped as too old or unreadable, and pruning s3_history never resets it.
CREATE TABLE s3_cursors (
    bucket     text        NOT NULL,
    prefix     text        NOT NULL,
    last_key   text        NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (bucket, prefix)
);

CREATE TABLE run_changes (
    run_id    bigint NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    source    text   NOT NULL CHECK (source IN ('s3', 'log')),
    counts    jsonb  NOT NULL DEFAULT '{}',
    resources jsonb,
    PRIMARY KEY (run_id, source)
);

-- +goose Down
DROP TABLE run_changes;
DROP TABLE s3_cursors;
DROP TABLE s3_history;
UPDATE runs SET commit_source = 'update' WHERE commit_source = 'history';
ALTER TABLE runs DROP CONSTRAINT runs_commit_source_check;
ALTER TABLE runs ADD CONSTRAINT runs_commit_source_check
    CHECK (commit_source IN ('', 'update', 'stack'));
ALTER TABLE stacks DROP COLUMN s3_checked_at, DROP COLUMN s3_error, DROP COLUMN pulumi_stack,
    DROP COLUMN project, DROP COLUMN backend_url;
