-- +goose Up
-- One app can read several state buckets, so a history key is only unique per bucket.
ALTER TABLE s3_history DROP CONSTRAINT s3_history_pkey;
ALTER TABLE s3_history ADD PRIMARY KEY (bucket, key);
-- Covered by the new primary key; no query filters history by key prefix any more.
DROP INDEX s3_history_prefix;
-- Lists show the first three changed resources and the count; keep them next to the full
-- array so a list row never reads resources (up to 1 MiB per run).
ALTER TABLE run_changes
    ADD COLUMN summary jsonb NOT NULL DEFAULT '[]',
    ADD COLUMN resource_total integer NOT NULL DEFAULT 0;
UPDATE run_changes SET
    summary = COALESCE((SELECT jsonb_agg(jsonb_build_object('type', r->>'type', 'name', r->>'name'))
                        FROM (SELECT r FROM jsonb_array_elements(resources) r LIMIT 3) x), '[]'),
    resource_total = jsonb_array_length(resources)
WHERE resources IS NOT NULL;

-- +goose Down
ALTER TABLE run_changes DROP COLUMN resource_total, DROP COLUMN summary;
DELETE FROM s3_history a USING s3_history b WHERE a.key = b.key AND a.bucket > b.bucket;
ALTER TABLE s3_history DROP CONSTRAINT s3_history_pkey;
ALTER TABLE s3_history ADD PRIMARY KEY (key);
CREATE INDEX s3_history_prefix ON s3_history (bucket, key);
