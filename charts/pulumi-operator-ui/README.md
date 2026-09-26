# pulumi-operator-ui chart

Installs [pulumi-operator-ui](https://github.com/jalet/pulumi-operator-ui), a
read-only UI for Pulumi Kubernetes Operator stacks.

```sh
helm install pou oci://ghcr.io/jalet/helm-charts/pulumi-operator-ui -f values.yaml
```

The chart renders a single-replica Deployment, a Service (`http` 8080,
`metrics` 9090), a ServiceAccount with read-only RBAC, a NetworkPolicy and,
optionally, an HTTPRoute and a ServiceMonitor. It creates no Secrets: point it
at existing ones.

Required values are listed in the project
[README](https://github.com/jalet/pulumi-operator-ui#install); every value is
documented in [values.yaml](values.yaml).

The HTTPRoute sends `/events` through a rule with `timeouts.request: 0s`, so
the live-update stream is not cut by the Gateway's request timeout.
