-- +goose Up
-- pulumi-operator-ui/watches on a preview (drift detector) Stack: NULL when absent, '' to opt
-- out of pairing with the Stack it checks, else "name" or "namespace/name" of that Stack.
ALTER TABLE stacks ADD COLUMN watches text;

-- +goose Down
ALTER TABLE stacks DROP COLUMN watches;
