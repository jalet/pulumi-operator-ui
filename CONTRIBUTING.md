# Contributing

Issues and pull requests are welcome. For anything larger than a small fix, open an issue first
so the approach can be agreed before you spend time on it.

## Setup

Tools are pinned in `mise.toml` and `mise.lock`:

```sh
mise install
mise run test        # needs Docker or Podman for the PostgreSQL tests
```

The README's Development section shows how to run the app locally against kind and the stub
IdP.

## Making a change

- Write the test first. Store tests run against real PostgreSQL (testcontainers), the watch
  against envtest with the PKO CRDs, and the web and auth packages against `httptest` and a stub
  OIDC provider.
- Before pushing, run what CI runs:

  ```sh
  mise run css:check && mise run lint && mise run test && mise run vuln && mise run chart:test
  ```

- After changing templates or `web/styles/input.css`, run `mise run css` and commit the rebuilt
  `internal/web/static/app.css`. Colors belong in the token blocks of `input.css`; a test fails
  on colors anywhere else.
- A schema change is a new migration in `internal/store/migrations`, with a working Down.
- Keep the app read-only: it must never gain a write verb on the cluster.

## Commits and pull requests

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org)
(`feat(web): ...`, `fix(store): ...`). A pull request describes what changed and why, and how
it was verified.

## Releases

Releases are cut with [release-please](https://github.com/googleapis/release-please). It keeps
a release PR open that collects the conventional commits on `main` into `CHANGELOG.md` and
bumps the version in `charts/pulumi-operator-ui/Chart.yaml` (a `feat` bumps the minor version
before 1.0, a `fix` the patch). Merging that PR tags `vX.Y.Z` and creates the GitHub release;
the same workflow then builds the image with ko, pushes the image and the chart to ghcr.io,
and signs both with cosign.

## License

By contributing you agree that your contribution is licensed under the
[Apache License 2.0](LICENSE).
