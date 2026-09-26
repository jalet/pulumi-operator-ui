# Changelog

All notable changes are recorded here. The project follows
[Semantic Versioning](https://semver.org).

## [0.2.0](https://github.com/jalet/pulumi-operator-ui/compare/v0.1.0...v0.2.0) (2026-09-26)


### Features

* fold drift detectors into the stack they check ([c776990](https://github.com/jalet/pulumi-operator-ui/commit/c7769900351a0e2aa5be9ad119f10f22b37855bd))
* **store:** record which Stack a drift detector watches ([e6c4a50](https://github.com/jalet/pulumi-operator-ui/commit/e6c4a50c2197bc0adb55e56e87d2d720add0a76b))
* **web:** fold drift detectors into the stack they check ([31fcb60](https://github.com/jalet/pulumi-operator-ui/commit/31fcb60f793beac41afdcc48ed805a4be9315acc))
* **web:** show the drift detector on the stack it checks ([1845d4a](https://github.com/jalet/pulumi-operator-ui/commit/1845d4a5d86d091823d9449e846d367d61967175))


### Bug fixes

* **store:** reshape the list when a drift pairing changes ([a4182f4](https://github.com/jalet/pulumi-operator-ui/commit/a4182f4fd8fe4483487c3dc046bc74bbdbe6547a))
* **web:** fold detectors across namespace filters, keep columns aligned ([356953a](https://github.com/jalet/pulumi-operator-ui/commit/356953aa42142f200baece2da6f6c8e4409f7644))

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
