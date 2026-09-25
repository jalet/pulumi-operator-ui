#!/usr/bin/env bash
# Pins the PKO CRDs used by envtest. Source: the operator's generated CRD bases at the
# release tag, https://github.com/pulumi/pulumi-kubernetes-operator/tree/v2.9.1/operator/config/crd/bases
set -euo pipefail
version="v2.9.1"
dest="test/crds/pko-2.9.1"
base="https://raw.githubusercontent.com/pulumi/pulumi-kubernetes-operator/${version}/operator/config/crd/bases"
mkdir -p "$dest"
for f in pulumi.com_stacks.yaml auto.pulumi.com_updates.yaml; do
  curl -fsSL "$base/$f" -o "$dest/$f"
  grep -q 'kind: CustomResourceDefinition' "$dest/$f"
done
