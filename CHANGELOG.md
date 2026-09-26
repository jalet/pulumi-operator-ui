# Changelog

All notable changes are recorded here. The project follows
[Semantic Versioning](https://semver.org).

## 0.1.0 (2026-09-26)

The first public release.

### Features

- Stack list with readiness, last deploy, commit and success rate, updating live over
  server-sent events.
- Per-stack timeline of state changes (up, refresh, destroy, import), numbered from Pulumi's
  history, with drift previews folded in between; cursor paging in both directions and a choice
  of page size.
- Run pages with the changed resources and their property diffs, read from the workspace pod's
  engine log; credential-like values are redacted, and `logs.diffs=false` keeps counts only.
- Optional S3 history for DIY backends: exact commits, change counts, runs the app never saw
  (including CLI updates and imports), and whether a run came from the operator or a laptop.
- OIDC sign-in with a claim allowlist, sign-out at the IdP, and sign-in event recording.
- Configurable colors per light and dark mode, with a built-in palette.
- The running version, commit and build time in every page footer and in `pou_build_info`.
- Helm chart with a default-deny NetworkPolicy, a values schema, and signed releases.
