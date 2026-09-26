# pulumi-operator-ui

A read-only web UI for the
[Pulumi Kubernetes Operator](https://github.com/pulumi/pulumi-kubernetes-operator) (PKO v2).
PKO runs your Pulumi programs from `Stack` resources but ships no UI, and the `Update` objects
that record each run are garbage-collected. This app keeps that history and shows it:

- every Stack with its readiness, last deploy, commit and recent success rate;
- a per-stack timeline of state changes (up, refresh, destroy, import), numbered like Pulumi's
  own update counter, with the drift previews folded in between;
- for each run, which resources changed and how, read from the engine log in the workspace pod
  (credential-like values redacted);
- optionally, Pulumi's own update history from S3 DIY backends: exact commits, change counts,
  and runs the app never saw;
- live updates over server-sent events, and the running release in every page footer.

It never writes to the cluster: no triggering, approving or cancelling runs.

![A stack page: numbered runs by day, ups without changes grouped into ranges, a drift preview, a failed run and a laptop run with its commit subject](docs/images/stack.png)

## Requirements

- Kubernetes with PKO 2.x (tested with 2.9.1).
- PostgreSQL (tested with 17), reachable over verified TLS (a
  [CloudNativePG](https://cloudnative-pg.io) cluster works).
- An OIDC provider (Keycloak, Entra ID, Dex, ...) with a confidential client.

## Install

```sh
helm install pou oci://ghcr.io/jalet/helm-charts/pulumi-operator-ui \
  --namespace pulumi-operator-ui --create-namespace -f values.yaml
```

Required values:

| Value | Meaning |
|---|---|
| `database.urlSecret.name` | Secret with the PostgreSQL URL under key `uri` (a CNPG app secret works, together with `database.caSecret`) |
| `database.caSecret.name` | CA for the database's TLS certificate (for CNPG: `<cluster>-ca`, key `ca.crt`); required unless the URL has `sslmode=verify-full` |
| `oidc.issuer`, `oidc.clientID`, `oidc.redirectURL` | OIDC client settings; the redirect URL ends in `/auth/callback`, and both must be https |
| `oidc.clientSecret.name` | Secret with the client secret under key `client-secret` |
| `auth.allowed` | Values of `auth.claim` (default `groups`) that may sign in |
| `session.keySecret.name` | Secret with at least 32 bytes of session key under key `key` (`openssl rand -base64 48`) |
| `networkPolicy.apiServer.cidrs` | API server endpoint CIDRs, or set `networkPolicy.enabled=false` |

Recommended:

- **`namespaces`:** list the namespaces your Stacks live in. Left empty, the app watches every
  namespace through a ClusterRole, which also lets it read pod logs cluster-wide.
- **`networkPolicy`:** set the `gateway`, `database` and `metricsScraper` selectors and narrow
  the `idp` and `s3` CIDRs. The defaults allow every pod to reach the UI and metrics ports and
  allow HTTPS egress anywhere.
- **Sign-out:** logout also ends the IdP session when the IdP advertises
  `end_session_endpoint`. Register the app root, `https://<host>/`, as a valid post-logout
  redirect URI for the client (in Keycloak: "Valid post logout redirect URIs"), or set
  `oidc.localLogout=true` to sign out of the app only.

Everything else is in [the chart's values](charts/pulumi-operator-ui/values.yaml), and every
flag is listed in [docs/design.md](docs/design.md#configuration).

## S3 history (optional)

For Stacks on an S3 DIY backend, the app can read Pulumi's update history to show exact
commits, change counts, and runs whose `Update` was garbage-collected or that ran before the
app was installed (including `pulumi up` and `pulumi import` from a laptop). It is off by
default and needs:

1. An IAM identity with only [`docs/iam/s3-history-policy.json`](docs/iam/s3-history-policy.json)
   (substitute `BUCKET` and `PREFIX`, for example `state-bucket` and `pulumi/example`; for a
   backend without a path, `s3://bucket`, drop `PREFIX/`). It lists the history prefix and reads
   `*.history.json` and `*.history.json.gz` files only, never the `.checkpoint.json` state, and
   denies plain HTTP. With a customer-managed KMS key on the bucket, add `kms:Decrypt` on that
   key, conditioned on `kms:ViaService = s3.<region>.amazonaws.com`.
2. Credentials: either static keys in a Secret with keys `access-key-id` and
   `secret-access-key` (`s3History.credentialsSecret.name`), or the pod's own identity with
   `s3History.ambientCredentials=true` (EKS Pod Identity, or IRSA through
   `serviceAccount.annotations`).
3. `s3History.enabled=true`. `s3History.region` sets the region for backend URLs without
   `?region=`.

Each Stack's bucket, prefix and region come from its `spec.backend`; Stacks on other backends
are skipped. Problems show on the Stack's page and in `pou_s3_errors_total`, and never affect
readiness. A deleted or archived history file is passed over; access denied holds that Stack's
history until the permission is fixed. The legacy non-project layout
(`PULUMI_DIY_BACKEND_LEGACY_LAYOUT`) is not supported.

## Colors

The UI follows the OS light or dark setting. Every color is a token, and the chart's `theme`
value overrides any of them per mode; keys left out keep the built-in palette:

```yaml
theme:
  light:
    page: "#ffffff"
    bad: "#d93025"
  dark:
    page: "#000000"
  brandBar: ["#4a7c9b", "#5a8fa8", "#6aa0b8", "#c8a84e", "#d8b85e"]
```

Keys for `light` and `dark`: `page`, `panel`, `line`, `ink`, `muted`, `focus`, `ok`, `run`,
`attention`, `bad`, `neutral`, `okText`, `runText`, `attentionText`, `badText`, `neutralText`.
Values are quoted hex colors (`#rgb`, `#rrggbb` or `#rrggbbaa`); an unquoted `#fff` is a YAML
comment. The chart and the app both reject unknown keys and bad values. Changing the theme
restarts the pod.

## Security model

- **Read-only.** RBAC grants `get`, `list` and `watch` on `stacks` and `updates` and `get` on
  `pods/log`; the app never reads Secrets.
- **Who sees what.** Every page requires an OIDC session, and access is an allowlist on one
  claim: everyone allowed sees every watched namespace. Stored diffs keep Pulumi's `[secret]`
  masking and redact credential-like keys, but a value Pulumi was not told is secret (a plain
  config value passed into a Helm chart, say) can still appear. If the allowlist is wider than
  the people who may read deployment details, set `logs.diffs=false` to keep only counts and
  resource names.
- **Sessions** are signed cookies that expire after `session.maxAge` (default 8h). They are not
  stored server-side: removing someone from the allowed group takes effect at their next
  sign-in. To end every session now, rotate the session key without keeping the previous one.
- **Sign-in events** (subject, email, claim values) are kept 30 days by default
  (`retention.auth`).

More detail, including the HTTP headers, database TLS rules and container hardening:
[docs/design.md](docs/design.md#security-model). To report a vulnerability, see
[SECURITY.md](SECURITY.md).

## Verifying a release

Images and charts are signed with cosign (keyless, by the release-please workflow on `main`):

```sh
cosign verify ghcr.io/jalet/helm-charts/pulumi-operator-ui:<version> \
  --certificate-identity-regexp '^https://github.com/jalet/pulumi-operator-ui/\.github/workflows/release-please\.yml@refs/heads/main$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
cosign verify ghcr.io/jalet/pulumi-operator-ui:<version> \
  --certificate-identity-regexp '^https://github.com/jalet/pulumi-operator-ui/\.github/workflows/release-please\.yml@refs/heads/main$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Development

Tools are pinned in `mise.toml`:

```sh
mise install
mise run lint        # golangci-lint
mise run test        # unit, envtest and testcontainers tests (needs Docker or Podman)
mise run vuln        # govulncheck
mise run chart:test  # helm lint + chart rendering tests
mise run css         # rebuild internal/web/static/app.css after changing templates or web/styles
mise run build       # ko build into the local image store
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

Open <http://localhost:8080>. Browsers accept `Secure` cookies on `http://localhost`.

See [CONTRIBUTING.md](CONTRIBUTING.md) for how changes are made and released.

## License

[Apache License 2.0](LICENSE).
