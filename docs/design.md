# Architecture

pulumi-operator-ui is a read-only web UI for the
[Pulumi Kubernetes Operator](https://github.com/pulumi/pulumi-kubernetes-operator) (PKO) v2.
This document describes how it works: what it reads, what it stores, how pages stay live, and
where its security boundaries are. The [README](../README.md) covers installing and running it.

## What PKO exposes

PKO reconciles `Stack` resources by running Pulumi in a workspace pod and recording each
operation as an `Update`. It ships no UI, and completed `Update` objects are garbage-collected,
so their history disappears.

| Object | Fields the app reads |
|---|---|
| `Stack` (`pulumi.com/v1`) | `status.conditions` (Ready, Reconciling, Stalled); `status.lastUpdate` (name, type, state, message, commits); `status.currentUpdate`; `spec.backend`, `spec.stack`, `spec.projectRepo`; `status.projectInfo` |
| `Update` (`auto.pulumi.com/v1alpha1`) | `spec.type`; `status.conditions`, `startTime`, `endTime`, `message` |
| Workspace pod (`<stack>-workspace-0`) | its log (`pods/log`), which carries the Pulumi engine output |

`Update.status` has no change summary. Which resources changed is only in the engine output
and, for S3 DIY backends, as counts in Pulumi's own `.pulumi/history/<project>/<stack>/`
files.

## Components

```
            Kubernetes API                           S3 bucket (optional)
   Stack / Update / pods/log                         .pulumi/history/*.history.json
        |                 |                                   |
  internal/watch    internal/logs                       internal/s3hist
        |                 |                                   |
  internal/record         |                                   |
        +-----------------+------> internal/store <-----------+
                                         |    \
                                         |     internal/events (in-process broker)
                                         |              |
                          internal/web (pages, fragments, /events SSE) <-- internal/auth (OIDC)
                                         |
                                 browser (htmx + SSE)
```

| Package | Responsibility |
|---|---|
| `internal/watch` | A controller-runtime cache of Stacks and Updates, in all namespaces or a configured list; reconciles each change into the store |
| `internal/record` | Maps a Stack or Update to store rows. An Update has no commit field, so the commit comes from the owning Stack: exact when `status.currentUpdate` or `status.lastUpdate` names this Update, approximate (shown dotted) otherwise. Every reconcile also backfills the Stack's `status.lastUpdate` as a run, so runs whose Update was garbage-collected while the app was down still appear |
| `internal/logs` | For each finished run, reads its slice of the workspace pod log (between the run's start and end, bounded by the neighbouring runs on the same workspace), waits for the run's completed line, and parses the changed resources and the `Resources:` summary |
| `internal/s3hist` | Optional. Lists each Stack's history prefix page by page from a stored cursor, parses each new history file, and links it to a run by stack, type and time, or imports it as a run the app never saw |
| `internal/store` | PostgreSQL schema (embedded goose migrations), queries, retention pruning; publishes a change event after each committed change |
| `internal/events` | An in-process broker that fans change events out to SSE subscribers |
| `internal/auth` | OIDC login (authorization code with PKCE, `state`, `nonce`), signed session cookies, the claim allowlist, logout, sign-in event recording |
| `internal/web` | Pages and htmx fragments, `/events`, static assets, `/healthz` and `/readyz` |
| `internal/theme` | Parses the optional color theme and renders it as CSS custom properties |
| `internal/release` | The running build's version, commit and times, stamped by `.ko.yaml` |

`/metrics` is served on a separate listener (`--metrics-addr`).

## Data model

| Table | Holds |
|---|---|
| `stacks` | One row per Stack: readiness, last commit, backend, project, repo, S3 status; soft-deleted when the Stack is removed |
| `runs` | One row per run (preview, up, refresh, destroy, import), keyed by namespace and Update name: type, state, commit and its source, message, times, origin (operator or CLI), number (`seq`), log capture status |
| `run_changes` | Per run and source (`log` or `s3`): change counts, the changed resources with their diffs (log only), and a stored summary of the first three resources for lists |
| `s3_history` | One row per history file, keyed by bucket and key: parsed fields and its link state |
| `s3_cursors` | Per bucket and prefix: the last listed key and how many history keys precede it, which numbers runs |
| `auth_events` | Sign-ins, denials and errors: time, subject, email, outcome, claim values |

Runs are keyed by namespace and Update name, not UID: a run backfilled from
`Stack.status.lastUpdate` has no UID, and the Update seen later merges into the same row.
Upserts never move a terminal state back, keep the first non-empty commit unless an exact one
arrives, and write (and publish) nothing when nothing changed.

## Engine log capture

- A run becomes `pending` once it is finished and both of its times are known.
- The capture reads the workspace pod's log from the run's start, padded by 5 seconds for clock
  skew but never before the previous run on the workspace ended or after the next one started.
- It keeps only `pulumi` logger lines, stops at the run's `<op> completed` line, and retries
  (with backoff, for up to 10 minutes after the run ended) while that line has not appeared.
- The parser keeps each changed resource's type, name, URN and property diff, and the summary
  counts. It skips the `pulumi:pulumi:Stack` resource and the stack outputs.
- Values under keys that look like credentials (password, secret, token, private, access or API
  key) are stored as `[redacted]`, including whole maps and lists. `--logs.diffs=false` stores
  only counts and resource names.
- Caps: 4 MiB read per run, 1 MiB stored per run, 64 KiB per diff, 2000 resources.

## S3 history

Off by default (`--s3-history.enabled`). When on, each Stack's bucket, prefix and region come
from its `spec.backend`; S3 compatible endpoints (`?endpoint=`) are not supported.

- Needs `s3:ListBucket` on the history prefix and `s3:GetObject` on
  `*.history.json` and `*.history.json.gz` only, never the `.checkpoint.json` state files
  ([policy](iam/s3-history-policy.json)).
- From each file it stores the kind, times, result, change counts, the commit
  (`environment["git.head"]`), the update message, the execution origin (`exec.kind`,
  `exec.agent`) and the repository (`vcs.*`). It never decodes `config` or any other
  `environment` field.
- A history file that is deleted or archived is passed over and counted; access denied stops
  that Stack's history (it usually means a missing permission for the whole bucket) and shows
  on the Stack's page.
- Files are capped at 1 MiB, compressed and expanded.
- The legacy non-project layout (`PULUMI_DIY_BACKEND_LEGACY_LAYOUT`) is not supported.

## Live updates

The store publishes an event after each committed change. `/events` streams them as SSE, and
htmx re-fetches the affected fragment: a stack row, a run row, a run header, or a stack's
timeline. A viewer may hold 8 streams and the process 256; each write has a 10 second deadline,
and a stream ends when its session expires.

The broker is in-process, so the app runs as a single replica. While the pod restarts only the
UI is unavailable: the watch re-lists on start and converges.

## Retention

`--retention` (default 180 days) prunes `runs`, their `run_changes` and `s3_history`.
`--auth-retention` (default 30 days) prunes `auth_events`. Sign-in events hold personal data
(subject, email and claim values), so the default is short; choose the period your own data
protection obligations call for.

A run backfilled from a Stack's `status.lastUpdate` is recorded again after it is pruned, for as
long as the Stack still reports it: it is that Stack's last run.

## Security model

- **Read-only.** RBAC grants only `get`, `list` and `watch` on `stacks` and `updates`, and `get`
  on `pods/log`. The app never reads Secrets, including the Update outputs Secret. Listing the
  namespaces to watch limits `pods/log` to them; watching all namespaces grants it
  cluster-wide.
- **Authentication.** Every page and stream requires an OIDC session. The issuer and the
  redirect URL must be https (http only for localhost). Access is an allowlist on one claim;
  everyone allowed sees everything. With `--auth.claim=email` only verified addresses count.
- **Sessions** are HMAC-signed `__Host-` cookies (`Secure`, `HttpOnly`, `SameSite=Lax`) that
  expire after `--session.max-age` (default 8h), also counted from when they were issued, so
  lowering the setting shortens existing sessions. Sessions are not stored server-side: to end
  every session at once, rotate the session key without keeping the previous one.
- **Logout** is a POST guarded against cross-origin requests. When the IdP advertises an end
  session endpoint, logout ends the IdP session too (`--oidc.local-logout` turns this off).
- **What viewers can see.** Pulumi masks values it knows are secret as `[secret]`. Stored diffs
  can still show values it does not know are secret; the redaction above covers credential-like
  keys only. Every allowlisted viewer sees every namespace's runs for the retention period, so
  consider `--logs.diffs=false` when the allowlist is wider than the people who may read
  deployment details.
- **HTTP.** A strict Content-Security-Policy (no inline script or style), `nosniff`,
  `Referrer-Policy: no-referrer`, `X-Frame-Options: DENY`, `Cross-Origin-Opener-Policy`,
  `Permissions-Policy`, and HSTS when the redirect URL is https. htmx is vendored with pinned
  checksums; nothing loads from a CDN. All output is escaped by `html/template`.
- **Database.** Off localhost the connection must use verified TLS (`sslmode=verify-ca` or
  `verify-full`, or a CA file), checked for every host the URL would dial.
- **Container.** Distroless, nonroot, read-only root filesystem, all capabilities dropped,
  `RuntimeDefault` seccomp; secret files are mounted with mode 0440.
- **Network.** The chart ships a default-deny NetworkPolicy with ingress from the Gateway and
  the metrics scraper and egress to the Kubernetes API, the database, the IdP and (when
  enabled) S3. Its default selectors and CIDRs are open; narrow them for your cluster.
- **Supply chain.** Release images and charts are signed with cosign (keyless, GitHub
  Actions); CI actions are pinned by commit; Dependabot and Renovate keep dependencies and the
  base image current.

## Configuration

| Flag | Default | Notes |
|---|---|---|
| `--namespaces` | all | comma-separated namespaces to watch |
| `--http-addr` | `:8080` | UI listener |
| `--metrics-addr` | `:9090` | `/metrics` listener |
| `--database-url` | required | falls back to `DATABASE_URL`; verified TLS off localhost |
| `--database.ca-file` | none | CA bundle for the database's certificate |
| `--oidc.issuer`, `--oidc.client-id`, `--oidc.client-secret-file`, `--oidc.redirect-url` | required | https except for localhost |
| `--oidc.ca-file` | none | extra CA bundle for the IdP |
| `--oidc.local-logout` | `false` | sign out of the app only; otherwise the IdP must accept `https://<host>/` as a post-logout redirect URI |
| `--auth.claim` | `groups` | the claim the allowlist matches |
| `--auth.allowed` | required | comma-separated allowed claim values |
| `--session.key-file` | required | at least 32 bytes |
| `--session.previous-key-file` | none | still accepted for verification during a key rotation |
| `--session.max-age` | `8h` | 5m to 24h |
| `--retention` | `4320h` | runs, their changes and S3 history |
| `--auth-retention` | `720h` | sign-in events |
| `--logs.diffs` | `true` | store engine log property diffs; `false` keeps counts and names |
| `--s3-history.enabled` | `false` | read Pulumi history from S3 DIY backends |
| `--s3-history.interval` | `5m` | at least `1m` |
| `--display-timezone` | `UTC` | IANA zone for the timeline's day headers |
| `--theme-file` | none | YAML color overrides; see the README |

## Error handling

| Failure | Behaviour |
|---|---|
| Database unreachable | `/readyz` fails; the watch and pollers retry |
| Kubernetes watch error | controller-runtime re-lists; upserts converge |
| Workspace pod log gone | the run is marked "log unavailable" and still shows its status |
| Log tail not flushed | capture retries for up to 10 minutes after the run ended |
| S3 error | shown on the Stack's page and counted in `pou_s3_errors_total`; never affects readiness |
| Claim not on the allowlist | a 403 page and a `denied` sign-in event |

## Testing

- Store: against PostgreSQL via testcontainers, including every migration up and back down
  with data in place.
- Watch and the end-to-end run: envtest with the PKO 2.9.1 CRDs.
- Logs and S3 history: fakes for the pod log source and S3, and golden engine logs.
- Web and auth: `httptest` with a stub OIDC provider (`internal/auth/oidctest`).
- Chart: `helm template` assertions and schema checks.
