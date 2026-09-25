-- +goose Up
CREATE TABLE stacks (
    namespace   text        NOT NULL,
    name        text        NOT NULL,
    ready       boolean     NOT NULL,
    reconciling boolean     NOT NULL,
    stalled     boolean     NOT NULL,
    last_commit text        NOT NULL DEFAULT '',
    updated_at  timestamptz NOT NULL,
    deleted_at  timestamptz,
    PRIMARY KEY (namespace, name)
);

CREATE TABLE runs (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    namespace     text        NOT NULL,
    update_name   text        NOT NULL,
    uid           text,
    stack_name    text        NOT NULL,
    type          text        NOT NULL CHECK (type IN ('preview', 'up', 'refresh', 'destroy')),
    commit        text        NOT NULL DEFAULT '',
    commit_source text        NOT NULL DEFAULT '' CHECK (commit_source IN ('', 'update', 'stack')),
    state         text        NOT NULL CHECK (state IN ('pending', 'running', 'succeeded', 'failed')),
    message       text        NOT NULL DEFAULT '',
    started_at    timestamptz,
    ended_at      timestamptz,
    observed_at   timestamptz NOT NULL,
    UNIQUE (namespace, update_name)
);
CREATE INDEX runs_stack_timeline
    ON runs (namespace, stack_name, (COALESCE(started_at, observed_at)) DESC, id DESC);
CREATE INDEX runs_retention ON runs ((COALESCE(ended_at, observed_at)));

CREATE TABLE auth_events (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at           timestamptz NOT NULL,
    subject      text        NOT NULL DEFAULT '',
    email        text        NOT NULL DEFAULT '',
    outcome      text        NOT NULL CHECK (outcome IN ('login', 'denied', 'error')),
    claim_values text[]      NOT NULL DEFAULT '{}',
    detail       text        NOT NULL DEFAULT ''
);
CREATE INDEX auth_events_at ON auth_events (at);

-- +goose Down
DROP TABLE auth_events;
DROP TABLE runs;
DROP TABLE stacks;
