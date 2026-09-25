-- +goose Up
-- spec.preview: a preview-only Stack never writes S3 history, so when several Stacks share one
-- Pulumi stack the poller attributes its history to the one that applies.
ALTER TABLE stacks ADD COLUMN preview boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE stacks DROP COLUMN preview;
