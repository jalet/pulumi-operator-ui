# pulumi-operator-ui design

- **Status:** proposed
- **Date:** 2026-09-25
- **First consumer:** the author's own clusters

## Context

The Pulumi Kubernetes Operator (PKO) v2 reconciles `Stack` CRs by creating `Workspace` pods and
`Update` objects. PKO ships no UI. The only ways to see what a preview or up changed are
`kubectl get update` and reading pod logs, and completed `Update` objects are
garbage-collected, so history disappears.

pulumi-operator-ui is a small, read-only web UI for tracking stacks, previews and deploys, in
the spirit of the Renovate Operator's dashboard. It is its own project with its own release
cycle, and it installs separately from the operator.

### What PKO exposes (2.9.1)

| Object | Useful fields |
|---|---|
| `Stack` (`pulumi.com/v1`) | `status.conditions` (Ready, Reconciling, Stalled); `status.lastUpdate` (name, type, state, message, `lastAttemptedCommit`, `lastSuccessfulCommit`, `lastResyncTime`) |
| `Update` (`auto.pulumi.com/v1alpha1`) | `spec.type`, `spec.ttlAfterCompleted`; `status.conditions` (Progressing, Failed, Complete), `startTime`, `endTime`, `message`, `outputs` (Secret name) |
| `Workspace` (`auto.pulumi.com/v1alpha1`) | the pod that runs the program |

`Update.status` has no change summary. Per-resource changes are visible only in the engine
output (the logs) and, for S3 DIY backends, in `.pulumi/history/<project>/<stack>/*.history.json`.

## Goals

- List every Stack with its readiness, last preview, last up, commit and age.
- A per-stack timeline of runs (preview, up, refresh, destroy) with state, duration and commit.
- A per-run page showing change counts, the resource list, the operator message and the raw
  log.
- History that survives `Update` GC and pod restarts.
- Live updates without reloading the page.

## Non-goals

- Any write to the cluster: no triggering, approving or cancelling runs.
- Decrypting secrets or reading stack checkpoints.
- Multi-cluster support in v1. One deployment watches one cluster.

## Decisions

| Topic | Decision |
|---|---|
| Access to the cluster | Read-only: get/list/watch on stacks and updates (phase 1); phase 2 adds get on `pods` and `pods/log` |
| Language and UI | Go, `html/template` + htmx, live refresh over SSE; one binary, no Node toolchain |
| UI styling | Tailwind CSS v4 via its standalone CLI (pinned in mise); the compiled `app.css` is committed and CI checks it is current. Playground brand theme, following the OS light or dark setting; fonts self-hosted |
| Persistence | PostgreSQL (pgx, embedded migrations). example runs a dedicated CNPG cluster for it |
| Authentication | Built-in OIDC (go-oidc, x/oauth2), custom CA bundle supported, HMAC-signed session cookie |
| Authorization | Allowlist on a configurable claim (groups or roles). Everyone on the allowlist sees everything |
| Change details | Workspace pod logs, always on. S3 history, **opt-in**, off by default |
| Packaging | ko-built distroless nonroot image `ghcr.io/jalet/pulumi-operator-ui`; Helm chart `oci://ghcr.io/jalet/helm-charts/pulumi-operator-ui`; both public |

## Architecture

```
            Kubernetes API                         S3 bucket (optional)
   Stack / Update / Workspace / Pod / pods/log     .pulumi/history/*.json
                 |                                        |
          internal/watch ----> internal/record      internal/s3hist
                 |                    |                   |
          internal/logs --------------+---> internal/store <+
                                               |
                                         internal/web  <-- internal/auth (OIDC)
                                               |
                                    browser (htmx + SSE)
```

| Package | Responsibility | Depends on |
|---|---|---|
| `internal/watch` | controller-runtime cache watching Stack, Update, Workspace and workspace Pods; namespaces configurable, all by default | Kubernetes API |
| `internal/record` | Maps Update and Stack events to idempotent upserts on `runs`: Update UID, stack, type, commit, start, end, state, message. The Update CRD carries no commit, so the commit comes from the owning Stack: `status.currentUpdate.commit` or `status.lastUpdate.lastAttemptedCommit` when that entry names this Update (`commit_source=update`, exact), otherwise `lastAttemptedCommit` when the Update is first seen (`commit_source=stack`, shown as approximate). A restart re-lists and converges, and every Stack reconcile inserts its `status.lastUpdate` as a run if that run is not recorded yet | watch, store |
| `internal/logs` | The workspace pod is expected to be long-lived and reused across Updates (to be confirmed in the spike), so the log is sliced per run: read with `sinceTime` set to the Update's `startTime`, stop at `endTime`. If the app restarts mid-run, it re-reads from `startTime` and replaces the stored text, so capture is idempotent. Stores ANSI-stripped text, capped at 1 MiB, then parses the resource lines and the `Resources:` summary with a pure parser | watch, store |
| `internal/s3hist` | **Optional.** Only built when `--s3-history.enabled=true`; nothing else imports it. Every 5 minutes, lists history files newer than the last one seen (`StartAfter`) and stores `kind`, timestamps, `result`, the `resourceChanges` counts and `git.head`, matching each entry to an `up`, `refresh` or `destroy` run of the same stack by time window (see S3 history findings). DIY history does not record previews, so preview runs never have S3 counts | S3, store |
| `internal/store` | Schema, queries and retention: `--retention` (default 180 days) for runs, logs and changes; `--auth-retention` (default 1 year) for auth events | PostgreSQL |
| `internal/auth` | OIDC login and callback (authorization code with PKCE, `state` and `nonce`), sessions with a fixed lifetime, claim allowlist middleware, auth event recording | IdP, store |
| `internal/web` | Pages, `/events` SSE stream, `/healthz`, `/readyz`; `/metrics` on a separate listener (`--metrics-addr`) | store, auth |

### Data model

| Table | Key columns |
|---|---|
| `stacks` | namespace, name, ready, reconciling, stalled, last_commit (`lastSuccessfulCommit`), updated_at, deleted_at |
| `runs` | id (identity), namespace, update_name (unique with namespace), uid (Update UID, null when backfilled), stack_name, type, commit, commit_source (`update` or `stack`), state, message, started_at, ended_at, observed_at, log_status (`''`, `pending`, `captured`, `unavailable`) |
| `run_changes` | run_id, source (`log` or `s3`), create, update, delete, replace, same, resources (jsonb) |
| `auth_events` | at, subject, email, outcome (`login`, `denied`, `error`), claim_values |

Runs are keyed by namespace and Update name, not UID: a run backfilled from `Stack.status.lastUpdate` has no UID, and the Update seen later merges into the same row. Upserts never move a terminal state back to running, and keep the first non-empty commit unless an exact (`update`) commit arrives.

The run page shows `s3` counts when they exist and otherwise falls back to `log`.

A Stack removed from the cluster is soft-deleted (`deleted_at` set) and hidden from the default
list. Its runs stay and follow normal retention. The row is purged once no runs remain.

### Retention

`--retention` (default 180 days) prunes `runs` and `run_changes`. `--auth-retention`
(default 1 year) prunes `auth_events`, following the internal logging baseline (COMP-008).
Auth events contain personal data (subject, email), so this retention period must be justified
under GDPR Article 5(1)(e), storage limitation. VERIFY WITH LEGAL COUNSEL.

### Known limits

- If the app is down for longer than an Update's `ttlAfterCompleted`, that Update is gone
  before it is recorded. The startup backfill from `Stack.status.lastUpdate` recovers only the
  latest run per stack, and runs recovered this way have `log_status=missing`.

### S3 history is opt-in

- It is off by default: `--s3-history.enabled=false`, chart value `s3History.enabled: false`.
- While off, the binary constructs no AWS client and reads no AWS config or credentials. The
  chart renders no AWS env and no S3 egress. The app runs fully on CR status plus logs.
- When on, it discovers each Stack's bucket, prefix and region from `spec.backend` and needs
  standard AWS SDK credentials (a dedicated read-only IAM user, never PKO's).
- The IAM permissions it needs are `s3:ListBucket` conditioned on
  `s3:prefix` = `<prefix>/.pulumi/history/*`, and `s3:GetObject` on
  `<prefix>/.pulumi/history/*/*.history.json` only, which excludes the `.checkpoint.json`
  files (full stack state). `kms:Decrypt` is needed only when the bucket uses a
  customer-managed KMS key, conditioned on `kms:ViaService`; the AWS-managed `aws/s3` key
  needs no IAM KMS permission.
- It stores only `kind`, `startTime`, `endTime`, `result`, the `resourceChanges` counts and
  `environment["git.head"]`. It never stores `config` (it carries encrypted secret values) or
  any other `environment` field (it carries commit author and committer emails).
- Misconfiguration or S3 errors only mark runs with `s3_status=error` and increment a metric. They
  never fail readiness.

### Live updates

The store publishes change notifications on an in-process broker. `/events` streams them as SSE
and htmx swaps the affected fragments (stack row, run row, run header). A single replica is
assumed. If replicas are ever added, the broker would move to Postgres `LISTEN/NOTIFY`, but that
is not built in v1.

The single replica is an accepted availability risk for an internal read-only tool, and an
explicit exception to the multi-AZ baseline (ARCH-003). While the pod is down, only the UI is
unavailable. Runs are recovered on restart through the re-list, except those covered in Known
limits.

## Error handling

| Failure | Behaviour |
|---|---|
| Database unreachable | `/readyz` fails, and the watch loop retries with backoff |
| Kubernetes API watch error | controller-runtime re-lists, and record upserts converge |
| Pod gone before its log was read | `log_status=missing`; the run still shows CR status |
| Log exceeds 1 MiB | Truncated and flagged; parsing uses the kept part plus the tail summary if present |
| Parser finds no summary | `log_status=unparsed`; the raw log is still shown |
| S3 error (opt-in only) | `s3_status=error` plus a metric; log counts are still shown |
| OIDC claim not on the allowlist | 403 page, and the attempt is recorded in `auth_events` and logged |

## Security

- The app is read-only by RBAC: its ClusterRole contains no write verbs.
- It never reads Secrets, including the Update outputs Secret.
- Pulumi masks secret values as `[secret]` in engine output. Stored logs are still treated as
  sensitive: every page requires a session, and cookies are `Secure`, `HttpOnly`, `SameSite=Lax`.
- Sessions have a fixed absolute lifetime (`--session.max-age`, default 8h). The cookie is
  HMAC-signed, and a previous key (`--session.previous-key-file`) is accepted for verification
  so the key can be rotated without logging everyone out. The OIDC flow uses PKCE, `state` and
  `nonce`.
- htmx is vendored into the binary and nothing loads from a CDN. Responses set a strict
  `Content-Security-Policy`, `X-Content-Type-Options: nosniff` and `Referrer-Policy`. Stored logs
  are always rendered HTML-escaped.
- `/healthz` and `/readyz` are unauthenticated and return no data. `/metrics` is served on a
  separate listener (`--metrics-addr`) that is not exposed through the Gateway.
- It serves TLS only through the cluster Gateway, and the OIDC provider is trusted via a
  configurable CA bundle.
- The chart ships a NetworkPolicy: ingress only from the Gateway (and the metrics scraper on the
  metrics port), egress only to the Kubernetes API, the CNPG cluster, the IdP and, when S3
  history is enabled, S3.
- Data at rest: the CNPG cluster's volumes and its backups must be encrypted (an encrypted
  storage class, and an encrypted backup target with a KMS CMK where available). S3 history is
  read in the configured region, `eu-north-1` by default.
- The container runs nonroot with a read-only root filesystem. The service-account token is
  mounted only for the watch and is bound to the read-only ClusterRole.

## Configuration

| Flag | Default | Notes |
|---|---|---|
| `--namespaces` | all | comma-separated list to watch |
| `--http-addr` | `:8080` | UI listener |
| `--database-url` | required | falls back to the `DATABASE_URL` env var; `sslmode=disable` only for localhost |
| `--database.ca-file` | none | CA bundle for verifying the database's TLS certificate |
| `--oidc.issuer`, `--oidc.client-id`, `--oidc.client-secret-file`, `--oidc.redirect-url` | required | |
| `--oidc.ca-file` | none | extra CA bundle |
| `--auth.claim`, `--auth.allowed` | `groups`, required | e.g. `roles` / `Pulumi Viewers` |
| `--session.key-file` | required | HMAC key |
| `--session.previous-key-file` | none | accepted for verification during key rotation |
| `--session.max-age` | `8h` | absolute session lifetime |
| `--retention` | `4320h` | 180 days; runs, logs and changes |
| `--auth-retention` | `8760h` | 1 year; auth events |
| `--metrics-addr` | `:9090` | separate listener for `/metrics` |
| `--log.max-bytes` | `1048576` | |
| `--s3-history.enabled` | `false` | opt-in |
| `--s3-history.interval` | `5m` | poll interval, minimum `1m`; used only when enabled. Bucket and prefix come from each Stack's `spec.backend` |
| `--oidc.local-logout` | `false` | sign out of the app only, even when the IdP supports RP-initiated logout (which needs the app root registered as a post-logout redirect URI) |
| `--display-timezone` | `UTC` | IANA zone for the stack timeline's day headers; validated at start |
| `--theme-file` | empty | optional YAML color overrides (`light`, `dark`, `brandBar`), validated at start; the chart renders it from `theme` into a ConfigMap. Keys and defaults: `web/styles/input.css` between `tokens:start` and `tokens:end` |

## Open question for the spike (phase 0)

Where does PKO v2 write engine output: the workspace pod log, the operator controller log, or
neither in a parseable form? If it is only in the controller log, `internal/logs` follows the
controller pod instead and filters by Update name. If neither log has it, previews show status
only and the log feature is reduced to showing the raw workspace log. The spike should also
confirm:

- whether the workspace pod persists across Updates in 2.9.1, which decides the log slicing in
  `internal/logs`;
- answered in phase 1: the 2.9.1 Update CRD has no commit field; the Stack's
  `status.currentUpdate{name, commit}` names the running Update and its commit;
- answered by the S3 spike (2026-09-25): see "S3 history findings" below.

## S3 history findings (spike, 2026-09-25)

Probed read-only against the example backend
`s3://state-bucket/pulumi/example?region=eu-north-1` (AWS account 123456789012).

| Topic | Finding |
|---|---|
| Path | `<prefix>/.pulumi/history/<project>/<stack>/<stack>-<ns>.history.json`, here `pulumi/example/.pulumi/history/example-infra/prod/` |
| Neighbours | Every history file has a `<stack>-<ns>.checkpoint.json` next to it (about 300 KB, full state). Never read them |
| File name | `<ns>` is the entry's end time in Unix nanoseconds, fixed width, so lexical order is time order |
| Previews | Write no history (57 entries, none from the hourly previews) |
| `kind` | `update` (our `up`), `refresh`, and presumably `destroy` |
| Times | `startTime`, `endTime` in Unix seconds |
| `result` | `succeeded` or `failed` |
| `resourceChanges` | Counts only, by operation (for example `{create: 2, delete: 1, same: 108}`); no per-resource list, which only the engine log can give |
| Commit | `environment["git.head"]` is the exact commit |
| Sensitive fields | `config` (12 keys, encrypted secure values) and `environment` author and committer emails |
| Run link | No Update ID; entries match runs by stack, type and time only |
| Encryption | SSE-KMS with the AWS-managed `aws/s3` key, Bucket Keys enabled. All public-access blocks on, versioning on, bucket policy not public |

The AWS-managed key means the reader needs no `kms:Decrypt` in IAM; confirm on the first real read.
Operator baseline prefers a customer-managed key for state buckets; moving to one would add
`kms:Decrypt` with `kms:ViaService = s3.eu-north-1.amazonaws.com` to the policy.

## Phases

0. Spike (throwaway): answer the open question above.
1. Scaffold, watch, record, store, auth and a status-only UI; the chart, CI and the example
   deployment. This slice is usable on its own.
2. Log capture and parsing (done). The raw log is not stored:
   `run_changes` rows with `source = 'log'` hold counts and the changed resources.
3. Opt-in S3 history. For example, enabling it is a separate change: the IAM user, the AWS
   ExternalSecret and `s3History.enabled: true`.

## Testing

- Parser: golden files taken from real preview and up logs (captured in the spike).
- logs: slicing by `sinceTime` on a workspace pod reused across two Updates, and a restart
  mid-run that ends with the same stored log.
- store: against real Postgres via testcontainers.
- record and watch: envtest with PKO CRDs from the 2.9.1 chart.
- web and auth: httptest with a stub OIDC provider.
- s3hist: fake S3 client; plus a test that the binary starts with S3 history off and no AWS
  environment.
- Chart: `helm template` in CI with the S3 history value both off and on, asserting that the
  NetworkPolicy has no S3 egress and no AWS env is rendered when it is off.
