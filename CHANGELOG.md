# Changelog

All notable changes are recorded here. The project follows
[Semantic Versioning](https://semver.org).

## [0.2.2](https://github.com/jalet/pulumi-operator-ui/compare/v0.2.1...v0.2.2) (2026-10-10)


### Bug fixes

* **deps:** update aws-sdk-go-v2 monorepo ([#26](https://github.com/jalet/pulumi-operator-ui/issues/26)) ([806ca77](https://github.com/jalet/pulumi-operator-ui/commit/806ca77579f638d07762320123decb60fe6af5cd))
* **deps:** update aws-sdk-go-v2 monorepo ([#30](https://github.com/jalet/pulumi-operator-ui/issues/30)) ([ae6dfd8](https://github.com/jalet/pulumi-operator-ui/commit/ae6dfd82a1d4fcc5c127684a69462d9a13cf4142))
* **deps:** update aws-sdk-go-v2 monorepo ([#34](https://github.com/jalet/pulumi-operator-ui/issues/34)) ([46f10f3](https://github.com/jalet/pulumi-operator-ui/commit/46f10f3b7fdba02b3abc45ff2c0ad6e2ad3dec20))
* **deps:** update module github.com/aws/smithy-go to v1.28.3 ([#24](https://github.com/jalet/pulumi-operator-ui/issues/24)) ([2e2b932](https://github.com/jalet/pulumi-operator-ui/commit/2e2b93208072e48a941e2a10498c62c4e0bcc6be))
* **deps:** update module github.com/aws/smithy-go to v1.28.4 ([#27](https://github.com/jalet/pulumi-operator-ui/issues/27)) ([a462439](https://github.com/jalet/pulumi-operator-ui/commit/a462439ab5106168478f7e203988fedd68d66a55))
* **deps:** update module github.com/prometheus/client_golang to v1.25.0 ([#28](https://github.com/jalet/pulumi-operator-ui/issues/28)) ([1268075](https://github.com/jalet/pulumi-operator-ui/commit/126807583d2cba2383632d6323092f837c6b891f))
* **deps:** update module golang.org/x/sync to v0.24.0 ([#32](https://github.com/jalet/pulumi-operator-ui/issues/32)) ([9276c86](https://github.com/jalet/pulumi-operator-ui/commit/9276c864a5900d858d6dcc150c5c4626eba23266))

## [0.2.1](https://github.com/jalet/pulumi-operator-ui/compare/v0.2.0...v0.2.1) (2026-10-02)


### Bug fixes

* **deps:** update module github.com/aws/aws-sdk-go-v2/service/s3 to v1.114.0 ([#17](https://github.com/jalet/pulumi-operator-ui/issues/17)) ([c3feb12](https://github.com/jalet/pulumi-operator-ui/commit/c3feb120131d1b23e4f423ed272d6bbd939c9f42))
* **deps:** update module github.com/failsafe-go/failsafe-go to v0.9.8 ([#15](https://github.com/jalet/pulumi-operator-ui/issues/15)) ([708856f](https://github.com/jalet/pulumi-operator-ui/commit/708856fcf68d43c0a3af3b6a1c141555a3565d2b))
* **deps:** update module sigs.k8s.io/controller-runtime to v0.25.2 ([#18](https://github.com/jalet/pulumi-operator-ui/issues/18)) ([1bbb4ab](https://github.com/jalet/pulumi-operator-ui/commit/1bbb4abd29484814c3afd33e35ffa113fb06a8ee))

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
