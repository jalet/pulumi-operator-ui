# Security policy

## Supported versions

Security fixes land on `main` and ship in the next release. Only the latest release is
supported; upgrade to it before reporting.

## Reporting a vulnerability

Report vulnerabilities privately through GitHub:
**Security > Report a vulnerability** on
[github.com/jalet/pulumi-operator-ui](https://github.com/jalet/pulumi-operator-ui/security/advisories/new).
Please do not open a public issue.

Include what you found, how to reproduce it, and the version (shown in the page footer). You
can expect an acknowledgement within a week. Once a fix is released, the advisory is published
with credit to you, unless you prefer otherwise.

## Scope

In scope: the application, its Helm chart and its release artifacts (image and chart). Out of
scope: the Pulumi Kubernetes Operator itself, your identity provider, and findings that need
cluster-admin access to begin with.

The security model, including what viewers can see and what the app stores, is described in
[docs/design.md](docs/design.md#security-model).
