-- +goose Up
-- Update numbers: each history key's position among the prefix's keys, oldest first.
ALTER TABLE s3_cursors ADD COLUMN key_count bigint NOT NULL DEFAULT 0;
ALTER TABLE s3_history ADD COLUMN seq bigint;
ALTER TABLE runs ADD COLUMN seq bigint;
-- Re-list every prefix once so existing keys are numbered in order.
DELETE FROM s3_cursors;

-- +goose Down
ALTER TABLE runs DROP COLUMN seq;
ALTER TABLE s3_history DROP COLUMN seq;
ALTER TABLE s3_cursors DROP COLUMN key_count;
