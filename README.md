# pulumi-operator-ui

A small, read-only web UI for the
[Pulumi Kubernetes Operator](https://github.com/pulumi/pulumi-kubernetes-operator)
(PKO v2). It lists every `Stack` with its readiness, last preview and last up,
keeps a per-stack timeline of runs that survives `Update` garbage collection,
and refreshes live over server-sent events.

Phase 1 (this release) shows status from the `Stack` and `Update` resources.
Log capture (phase 2) and opt-in S3 history (phase 3) are described in
[docs/design.md](docs/design.md).

## Install

```sh
helm install pou oci://ghcr.io/jalet/charts/pulumi-operator-ui \
  --namespace pulumi-operator-ui --create-namespace -f values.yaml
```

Required values:

| Value | Meaning |
|---|---|
| `database.urlSecret.name` | Secret with the PostgreSQL URL under key `uri` (a CNPG app secret works, together with `database.caSecret`) |
| `database.caSecret.name` | CA for the database's TLS certificate (for CNPG: `<cluster>-ca`, key `ca.crt`); required unless the URL has `sslmode=verify-full` |
| `oidc.issuer`, `oidc.clientID`, `oidc.redirectURL` | OIDC client settings; the redirect URL ends in `/auth/callback` |
| `oidc.clientSecret.name` | Secret with the client secret under key `client-secret` |
| `auth.allowed` | Values of `auth.claim` (default `groups`) that may sign in |
| `session.keySecret.name` | Secret with at least 32 bytes of session key under key `key` (`openssl rand -base64 48`) |
| `networkPolicy.apiServer.cidrs` | API server endpoint CIDRs, or set `networkPolicy.enabled=false` |

Also set `networkPolicy.gateway`, `networkPolicy.database` and
`networkPolicy.metricsScraper` selectors for your cluster. Off localhost the app
refuses unverified database TLS, so a plain `sslmode=prefer` URL fails at startup. See
[the chart's values](charts/pulumi-operator-ui/values.yaml) for everything else.

## S3 history (optional)

For Stacks on an S3 DIY backend, the app can read Pulumi's own update history to show change
counts, an exact commit per run, and ups and refreshes it never saw (their Updates were
garbage-collected, or ran before the app was installed). It is off by default and needs:

1. An IAM user with only `docs/iam/s3-history-policy.json` (substitute `BUCKET` and `PREFIX`,
   for example `state-bucket` and `pulumi/example`). It lists the history prefix and reads
   `*.history.json` files only, never the `.checkpoint.json` state. With a customer-managed KMS
   key on the bucket, add `kms:Decrypt` on that key, conditioned on
   `kms:ViaService = s3.<region>.amazonaws.com`; the AWS-managed `aws/s3` key needs nothing.
2. Its keys in a Secret (for example via an ExternalSecret from your secret store) with keys
   `access-key-id` and `secret-access-key`.
3. `s3History.enabled=true` and `s3History.credentialsSecret.name=<secret>`.

The bucket, prefix and region come from each Stack's `spec.backend`; Stacks on other backends
are skipped. Problems per Stack show on its page ("Pulumi history unavailable: ...") and in
`pou_s3_errors_total`, and never affect readiness.

## Security model

- Read-only: the ClusterRole (or per-namespace Roles) grants only `get`,
  `list` and `watch` on `stacks.pulumi.com` and `updates.auto.pulumi.com`.
  It never reads Secrets.
- Every page requires an OIDC session (authorization code with PKCE, `state`
  and `nonce`). Access is an allowlist on one claim; everyone allowed sees
  everything. Sign-ins and denials are recorded in the `auth_events` table.
- Sessions are HMAC-signed `__Host-` cookies (`Secure`, `HttpOnly`,
  `SameSite=Lax`) with an absolute lifetime (`session.maxAge`, default 8h).
- Responses carry a strict Content-Security-Policy; htmx is vendored, nothing
  loads from a CDN.
- `/metrics` is served only on the separate metrics port.

Details: [docs/design.md](docs/design.md#security).

## Development

Tools are pinned in `mise.toml`:

```sh
mise install
mise run lint        # golangci-lint
mise run test        # unit, envtest and testcontainers tests (needs Docker or Podman)
mise run vuln        # govulncheck
mise run chart:test  # helm lint + chart rendering tests
mise run build       # ko build into the local image store
mise run css         # rebuild internal/web/static/app.css after changing templates or web/styles
```

Run it locally against kind, a throwaway Postgres and the stub IdP:

```sh
kind create cluster --name pou
kubectl apply -f test/crds/pko-2.9.1/
docker run -d --name pou-db -e POSTGRES_USER=pou -e POSTGRES_PASSWORD=pou -p 5432:5432 postgres:17-alpine
mise run devidp &    # stub IdP on http://127.0.0.1:5556, user "dev" in group "dev"
openssl rand -base64 48 > /tmp/pou-key; echo dev-secret > /tmp/pou-client-secret
DATABASE_URL='postgres://pou:pou@localhost:5432/pou?sslmode=disable' go run ./cmd/pulumi-operator-ui \
  --oidc.issuer=http://127.0.0.1:5556 --oidc.client-id=pou \
  --oidc.client-secret-file=/tmp/pou-client-secret \
  --oidc.redirect-url=http://localhost:8080/auth/callback \
  --auth.allowed=dev --session.key-file=/tmp/pou-key
```

Open <http://localhost:8080>. Browsers accept `Secure` cookies on
`http://localhost`.
