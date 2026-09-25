# Deferred minor findings

Minor findings from the final whole-branch reviews of phase 1, phase 3 (S3 history) and phase 2
(log capture), kept here when the plan workspaces were deleted. Each line names the effect and
where it lives. Items fixed later are marked with the commit that fixed them.

## Phase 1 (scaffold, watch, auth, UI)

- The run page does not listen to `sse:resync`, so after a broker overflow it can stay stale.
- Every reconcile publishes events even when no row changed.
- ~~`observed_at` bumps move runs without a start time to the top of the timeline and delay
  their retention.~~ Fixed in `06208dd`.
- Backfilled runs older than retention are pruned, then re-inserted from `Stack.status.lastUpdate`.
- The userinfo `sub` is not checked against the ID token `sub`; a userinfo outage is recorded as
  "denied" instead of "error".
- Logout has no CSRF token and no RP-initiated logout at the IdP.
- Concurrent logins in two tabs share one flow cookie, so one of them fails.
- Supply chain: the chart artifact is unsigned; `govulncheck` and `setup-envtest` are pinned to
  `latest`; the distroless base image is not digest-pinned.
- The "Seen" column shows reconcile time rather than when the run was first seen.
- The htmx login return target can be a fragment URL, so a user can land on a bare fragment.

## Phase 3 (S3 history)

- A permanent per-object `GetObject` error blocks that Stack's history; insert errors are
  labelled `reason=get`.
- An `AccessDenied` message on the stack page can include the IAM user ARN and account ID.
- The commit-upgrade `UPDATE` and `setLinkState` are not guarded in `WHERE` (harmless with one
  replica).
- The `s3_history` primary key is `key` only, not `(bucket, key)`.
- The history key `LIKE` may not use the index under a non-C collation.
- The down migration fails once `commit_source = 'history'` rows exist.
- `changeSummary` says "no changes" when only unknown operations are non-zero.
- A missing `resourceChanges` is stored as JSON `null`, not `{}`.
- The `s3_error` notice persists after S3 history is turned off.
- Chart: no `AWS_REGION`, static keys only (no pod identity), the values schema lacks
  `credentialsSecret`.
- README and IAM policy do not cover prefix-less backends; no optional `aws:SecureTransport` deny.
- Gzipped history (`.history.json.gz`) and legacy non-project backend layouts are unsupported and
  undocumented.

## Phase 2 (log capture)

- Two runs less than 5 s apart on one workspace could mix output (covered in practice by the
  completed-line handling in `81316fc`).
- An engine line over 1 MiB (`bufio.ErrTooLong`) is retried for 10 minutes instead of being
  treated as truncation.
- A read before the log tail is flushed could store a partial result as captured.
- The waiting note says "1 minutes" at a 1-minute interval and "0 minutes" below it, and does
  not check that the Stack is S3-backed.
- The live header swap closes open diffs and re-sends all diffs on every run event.
- A run only queues for capture when the finishing observation carries both times; a retry map
  entry leaks if a run is pruned during backoff; a URN without `::` loses its first character.
- The stack timeline shows only S3 counts, not engine log counts.
