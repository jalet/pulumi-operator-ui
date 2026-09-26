// Package chart tests the Helm chart by rendering it with helm template.
package chart

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/jalet/pulumi-operator-ui/internal/theme"
)

const _chart = "../../charts/pulumi-operator-ui"

func helmTemplate(t *testing.T, extra ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	args := append([]string{"template", "pou", _chart, "-n", "pou", "-f", "testdata/minimal.yaml"},
		extra...)
	var out bytes.Buffer
	cmd := exec.CommandContext(t.Context(), "helm", args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

func render(t *testing.T, extra ...string) []*unstructured.Unstructured {
	t.Helper()
	out, err := helmTemplate(t, extra...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	var objs []*unstructured.Unstructured
	for _, doc := range strings.Split(out, "\n---") {
		j, err := yaml.YAMLToJSON([]byte(doc))
		if err != nil {
			t.Fatalf("parse: %v\n%s", err, doc)
		}
		if string(j) == "null" || string(j) == "{}" {
			continue
		}
		obj := &unstructured.Unstructured{}
		if err := obj.UnmarshalJSON(j); err != nil {
			t.Fatalf("decode: %v\n%s", err, doc)
		}
		objs = append(objs, obj)
	}
	return objs
}

func find(objs []*unstructured.Unstructured, kind string) []*unstructured.Unstructured {
	var out []*unstructured.Unstructured
	for _, o := range objs {
		if o.GetKind() == kind {
			out = append(out, o)
		}
	}
	return out
}

func one(t *testing.T, objs []*unstructured.Unstructured, kind string) *unstructured.Unstructured {
	t.Helper()
	got := find(objs, kind)
	if len(got) != 1 {
		t.Fatalf("%d %s objects, want 1", len(got), kind)
	}
	return got[0]
}

func container(t *testing.T, objs []*unstructured.Unstructured) map[string]any {
	t.Helper()
	cs, _, _ := unstructured.NestedSlice(one(t, objs, "Deployment").Object,
		"spec", "template", "spec", "containers")
	if len(cs) != 1 {
		t.Fatalf("%d containers, want 1", len(cs))
	}
	c, ok := cs[0].(map[string]any)
	if !ok {
		t.Fatal("container is not an object")
	}
	return c
}

func args(t *testing.T, objs []*unstructured.Unstructured) []string {
	t.Helper()
	raw, _, _ := unstructured.NestedStringSlice(container(t, objs), "args")
	return raw
}

func rulesOf(t *testing.T, obj *unstructured.Unstructured) []map[string]any {
	t.Helper()
	raw, _, _ := unstructured.NestedSlice(obj.Object, "rules")
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			t.Fatal("rule is not an object")
		}
		out = append(out, m)
	}
	return out
}

func strs(v any) []string {
	raw, _ := v.([]any)
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		if str, ok := s.(string); ok {
			out = append(out, str)
		}
	}
	return out
}

func TestRBACReadOnly(t *testing.T) {
	role := one(t, render(t), "ClusterRole")
	got := map[string][]string{}
	for _, r := range rulesOf(t, role) {
		for _, verb := range strs(r["verbs"]) {
			if !slices.Contains([]string{"get", "list", "watch"}, verb) {
				t.Errorf("write verb %q in ClusterRole", verb)
			}
		}
		for _, g := range strs(r["apiGroups"]) {
			got[g] = append(got[g], strs(r["resources"])...)
		}
	}
	want := map[string][]string{"pulumi.com": {"stacks"}, "auto.pulumi.com": {"updates"},
		"": {"pods", "pods/log"}}
	for g, res := range want {
		if !slices.Equal(got[g], res) {
			t.Errorf("group %s resources = %v, want %v", g, got[g], res)
		}
	}
	for g, res := range got {
		if _, ok := want[g]; !ok || slices.Contains(res, "secrets") || slices.Contains(res, "*") {
			t.Errorf("unexpected access: group %q resources %v", g, res)
		}
	}
	one(t, render(t), "ClusterRoleBinding")
}

func TestNamespacedRBAC(t *testing.T) {
	objs := render(t, "--set", "namespaces={a,b}")
	if n := len(find(objs, "ClusterRole")); n != 0 {
		t.Fatalf("%d ClusterRoles in namespaced mode", n)
	}
	for _, kind := range []string{"Role", "RoleBinding"} {
		var nss []string
		for _, o := range find(objs, kind) {
			nss = append(nss, o.GetNamespace())
		}
		slices.Sort(nss)
		if !slices.Equal(nss, []string{"a", "b"}) {
			t.Errorf("%s namespaces = %v, want [a b]", kind, nss)
		}
	}
	for _, role := range find(objs, "Role") {
		core := 0
		for _, r := range rulesOf(t, role) {
			if !slices.Contains(strs(r["apiGroups"]), "") {
				continue
			}
			core++
			if !slices.Equal(strs(r["resources"]), []string{"pods", "pods/log"}) ||
				!slices.Equal(strs(r["verbs"]), []string{"get"}) {
				t.Errorf("%s core rule = %v", role.GetNamespace(), r)
			}
		}
		if core != 1 {
			t.Errorf("%s: %d core rules, want 1 (pods, pods/log)", role.GetNamespace(), core)
		}
	}
	if !slices.Contains(args(t, objs), "--namespaces=a,b") {
		t.Errorf("args = %v", args(t, objs))
	}
}

func TestPodHardening(t *testing.T) {
	objs := render(t)
	pod, _, _ := unstructured.NestedMap(one(t, objs, "Deployment").Object, "spec", "template", "spec")
	if v, _, _ := unstructured.NestedBool(pod, "securityContext", "runAsNonRoot"); !v {
		t.Error("pod runAsNonRoot is not true")
	}
	if v, _, _ := unstructured.NestedString(pod, "securityContext", "seccompProfile", "type"); v != "RuntimeDefault" {
		t.Errorf("seccompProfile = %q", v)
	}
	if v, _, _ := unstructured.NestedBool(pod, "automountServiceAccountToken"); !v {
		t.Error("automountServiceAccountToken is not true; the watch needs the token")
	}
	c := container(t, objs)
	if v, _, _ := unstructured.NestedBool(c, "securityContext", "readOnlyRootFilesystem"); !v {
		t.Error("readOnlyRootFilesystem is not true")
	}
	if v, found, _ := unstructured.NestedBool(c, "securityContext", "allowPrivilegeEscalation"); !found || v {
		t.Error("allowPrivilegeEscalation is not false")
	}
	drop, _, _ := unstructured.NestedStringSlice(c, "securityContext", "capabilities", "drop")
	if !slices.Equal(drop, []string{"ALL"}) {
		t.Errorf("capabilities.drop = %v", drop)
	}
	if n := len(find(objs, "Deployment")); n != 1 {
		t.Fatal("no deployment")
	}
	if r, _, _ := unstructured.NestedInt64(one(t, objs, "Deployment").Object, "spec", "replicas"); r != 1 {
		t.Errorf("replicas = %d, want 1", r)
	}
}

func TestSecretsAsFilesAndEnv(t *testing.T) {
	objs := render(t)
	c := container(t, objs)
	env, _, _ := unstructured.NestedSlice(c, "env")
	var dbFromSecret bool
	for _, e := range env {
		m, _ := e.(map[string]any)
		if m["name"] == "DATABASE_URL" {
			name, _, _ := unstructured.NestedString(m, "valueFrom", "secretKeyRef", "name")
			key, _, _ := unstructured.NestedString(m, "valueFrom", "secretKeyRef", "key")
			dbFromSecret = name == "pou-db-app" && key == "uri"
		}
	}
	if !dbFromSecret {
		t.Error("DATABASE_URL is not taken from the pou-db-app/uri secret")
	}
	a := args(t, objs)
	for _, want := range []string{"--oidc.client-secret-file=/etc/pou/secrets/client-secret",
		"--session.key-file=/etc/pou/secrets/session-key"} {
		if !slices.Contains(a, want) {
			t.Errorf("args lack %s: %v", want, a)
		}
	}
	mounts, _, _ := unstructured.NestedSlice(c, "volumeMounts")
	for _, m := range mounts {
		mm, _ := m.(map[string]any)
		if strings.HasPrefix(mm["mountPath"].(string), "/etc/pou") && mm["readOnly"] != true {
			t.Errorf("mount %v is not read-only", mm["mountPath"])
		}
	}
	if slices.ContainsFunc(a, func(s string) bool { return strings.HasPrefix(s, "--database.ca-file") }) {
		t.Error("--database.ca-file set without database.caSecret")
	}
	withCA := args(t, render(t, "--set", "database.caSecret.name=pou-db-ca"))
	if !slices.Contains(withCA, "--database.ca-file=/etc/pou/db-ca/ca.crt") {
		t.Errorf("args with caSecret = %v", withCA)
	}
}

func TestNoAWSEnv(t *testing.T) {
	out, err := helmTemplate(t)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "AWS_") {
		t.Error("chart renders AWS configuration while S3 history does not exist")
	}
}

func TestNetworkPolicy(t *testing.T) {
	np := one(t, render(t), "NetworkPolicy")
	types, _, _ := unstructured.NestedStringSlice(np.Object, "spec", "policyTypes")
	if !slices.Equal(types, []string{"Ingress", "Egress"}) {
		t.Fatalf("policyTypes = %v", types)
	}
	egress, _, _ := unstructured.NestedSlice(np.Object, "spec", "egress")
	ports := map[int64]bool{}
	var apiCIDR bool
	for _, rule := range egress {
		r, _ := rule.(map[string]any)
		ps, _, _ := unstructured.NestedSlice(r, "ports")
		for _, p := range ps {
			pm, _ := p.(map[string]any)
			if n, ok := pm["port"].(int64); ok {
				ports[n] = true
			}
		}
		to, _, _ := unstructured.NestedSlice(r, "to")
		for _, peer := range to {
			pm, _ := peer.(map[string]any)
			if cidr, _, _ := unstructured.NestedString(pm, "ipBlock", "cidr"); cidr == "10.0.0.1/32" {
				apiCIDR = true
			}
		}
	}
	for _, p := range []int64{53, 6443, 5432, 443} {
		if !ports[p] {
			t.Errorf("no egress on port %d", p)
		}
	}
	if !apiCIDR {
		t.Error("no egress to the API server CIDR")
	}
	ingress, _, _ := unstructured.NestedSlice(np.Object, "spec", "ingress")
	in := map[int64]bool{}
	for _, rule := range ingress {
		r, _ := rule.(map[string]any)
		ps, _, _ := unstructured.NestedSlice(r, "ports")
		for _, p := range ps {
			pm, _ := p.(map[string]any)
			if n, ok := pm["port"].(int64); ok {
				in[n] = true
			}
		}
	}
	if !in[8080] || !in[9090] || len(in) != 2 {
		t.Errorf("ingress ports = %v, want 8080 and 9090", in)
	}
}

func TestNetworkPolicyRequiresAPIServer(t *testing.T) {
	out, err := helmTemplate(t, "--set", "networkPolicy.apiServer.cidrs=null")
	if err == nil || !strings.Contains(out, "networkPolicy.apiServer.cidrs") {
		t.Fatalf("err = %v, output:\n%s", err, out)
	}
	if _, err := helmTemplate(t, "--set", "networkPolicy.enabled=false",
		"--set", "networkPolicy.apiServer.cidrs=null"); err != nil {
		t.Fatalf("disabled NetworkPolicy still requires cidrs: %v", err)
	}
}

func TestHTTPRoute(t *testing.T) {
	if n := len(find(render(t), "HTTPRoute")); n != 0 {
		t.Fatalf("%d HTTPRoutes rendered by default", n)
	}
	route := one(t, render(t, "--set", "httpRoute.enabled=true",
		"--set", "httpRoute.hostnames={pou.example}",
		"--set", "httpRoute.parentRefs[0].name=gw"), "HTTPRoute")
	rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
	var eventsRule, rootRule bool
	for _, rule := range rules {
		r, _ := rule.(map[string]any)
		matches, _, _ := unstructured.NestedSlice(r, "matches")
		for _, m := range matches {
			mm, _ := m.(map[string]any)
			path, _, _ := unstructured.NestedString(mm, "path", "value")
			timeout, _, _ := unstructured.NestedString(r, "timeouts", "request")
			switch path {
			case "/events":
				eventsRule = timeout == "0s"
			case "/":
				rootRule = true
			}
		}
		refs, _, _ := unstructured.NestedSlice(r, "backendRefs")
		for _, ref := range refs {
			rm, _ := ref.(map[string]any)
			if port, _ := rm["port"].(int64); port != 8080 {
				t.Errorf("backendRef port = %v, want only the http port", rm["port"])
			}
		}
	}
	if !eventsRule {
		t.Error("no /events rule with timeouts.request: 0s")
	}
	if !rootRule {
		t.Error("no / rule")
	}
}

func TestRequiredValues(t *testing.T) {
	for _, key := range []string{"database.urlSecret.name", "oidc.issuer", "oidc.clientID",
		"oidc.redirectURL", "oidc.clientSecret.name", "session.keySecret.name"} {
		out, err := helmTemplate(t, "--set", key+"=")
		if err == nil || !strings.Contains(out, key) {
			t.Errorf("empty %s: err = %v, output lacks the key:\n%s", key, err, out)
		}
	}
	out, err := helmTemplate(t, "--set", "auth.allowed=null")
	if err == nil || !strings.Contains(out, "auth.allowed") {
		t.Errorf("empty auth.allowed: err = %v\n%s", err, out)
	}
}

func TestServiceMonitorOptional(t *testing.T) {
	if n := len(find(render(t), "ServiceMonitor")); n != 0 {
		t.Fatalf("%d ServiceMonitors by default", n)
	}
	sm := one(t, render(t, "--set", "serviceMonitor.enabled=true"), "ServiceMonitor")
	eps, _, _ := unstructured.NestedSlice(sm.Object, "spec", "endpoints")
	if len(eps) != 1 || eps[0].(map[string]any)["port"] != "metrics" {
		t.Errorf("endpoints = %v", eps)
	}
}

// The image runs as the distroless nonroot user (65532); mounted secret files must be
// readable by it, and by nobody else.
func TestSecretFilesReadableByNonRootUser(t *testing.T) {
	objs := render(t)
	pod, _, _ := unstructured.NestedMap(one(t, objs, "Deployment").Object, "spec", "template", "spec")
	for _, field := range []string{"runAsUser", "runAsGroup", "fsGroup"} {
		if v, _, _ := unstructured.NestedInt64(pod, "securityContext", field); v != 65532 {
			t.Errorf("securityContext.%s = %d, want 65532", field, v)
		}
	}
	vols, _, _ := unstructured.NestedSlice(pod, "volumes")
	for _, v := range vols {
		vm, _ := v.(map[string]any)
		if vm["name"] != "secrets" {
			continue
		}
		if mode, _, _ := unstructured.NestedInt64(vm, "projected", "defaultMode"); mode != 0o440 {
			t.Errorf("secrets defaultMode = %#o, want 0440", mode)
		}
		return
	}
	t.Fatal("no secrets volume")
}

func TestS3HistoryOn(t *testing.T) {
	objs := render(t, "--set", "s3History.enabled=true", "--set", "s3History.credentialsSecret.name=pou-aws")
	a := args(t, objs)
	for _, want := range []string{"--s3-history.enabled=true", "--s3-history.interval=5m"} {
		if !slices.Contains(a, want) {
			t.Errorf("args lack %s: %v", want, a)
		}
	}
	env, _, _ := unstructured.NestedSlice(container(t, objs), "env")
	found := map[string]string{}
	for _, e := range env {
		m, _ := e.(map[string]any)
		name, _ := m["name"].(string)
		sec, _, _ := unstructured.NestedString(m, "valueFrom", "secretKeyRef", "name")
		key, _, _ := unstructured.NestedString(m, "valueFrom", "secretKeyRef", "key")
		found[name] = sec + "/" + key
	}
	if found["AWS_ACCESS_KEY_ID"] != "pou-aws/access-key-id" ||
		found["AWS_SECRET_ACCESS_KEY"] != "pou-aws/secret-access-key" {
		t.Errorf("AWS env = %v", found)
	}
	np := one(t, objs, "NetworkPolicy")
	egress, _, _ := unstructured.NestedSlice(np.Object, "spec", "egress")
	var s3Rule bool
	for _, rule := range egress {
		r, _ := rule.(map[string]any)
		to, _, _ := unstructured.NestedSlice(r, "to")
		ports, _, _ := unstructured.NestedSlice(r, "ports")
		for _, peer := range to {
			pm, _ := peer.(map[string]any)
			if cidr, _, _ := unstructured.NestedString(pm, "ipBlock", "cidr"); cidr == "0.0.0.0/0" {
				for _, p := range ports {
					if pp, _ := p.(map[string]any); pp["port"] == int64(443) {
						s3Rule = true
					}
				}
			}
		}
	}
	if !s3Rule || len(egress) != 5 {
		t.Errorf("S3 egress rule missing or wrong rule count (%d): %v", len(egress), egress)
	}
}

func TestS3HistoryOffHasNoS3Egress(t *testing.T) {
	egress, _, _ := unstructured.NestedSlice(one(t, render(t), "NetworkPolicy").Object, "spec", "egress")
	if len(egress) != 4 {
		t.Fatalf("egress rules = %d, want 4 without S3 history", len(egress))
	}
	if slices.ContainsFunc(args(t, render(t)), func(s string) bool { return strings.HasPrefix(s, "--s3-history") }) {
		t.Error("s3 history args rendered while off")
	}
}

func TestS3HistoryRequiresSecret(t *testing.T) {
	out, err := helmTemplate(t, "--set", "s3History.enabled=true")
	if err == nil || !strings.Contains(out, "s3History.credentialsSecret.name") {
		t.Fatalf("err = %v, output:\n%s", err, out)
	}
}
func TestDisplayTimezone(t *testing.T) {
	if a := args(t, render(t)); !slices.Contains(a, "--display-timezone=UTC") {
		t.Errorf("default args lack the timezone: %v", a)
	}
	if a := args(t, render(t, "--set", "displayTimezone=Europe/Stockholm")); !slices.Contains(a, "--display-timezone=Europe/Stockholm") {
		t.Errorf("args lack the configured timezone: %v", a)
	}
}

func themeValues(t *testing.T, doc string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "theme.yaml")
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestThemeOffByDefault(t *testing.T) {
	objs := render(t)
	if cms := find(objs, "ConfigMap"); len(cms) != 0 {
		t.Errorf("default chart renders %d ConfigMaps", len(cms))
	}
	for _, a := range args(t, objs) {
		if strings.HasPrefix(a, "--theme-file") {
			t.Errorf("default args carry %s", a)
		}
	}
	if strings.Contains(fmt.Sprint(container(t, objs)["volumeMounts"]), "/etc/pou/theme") {
		t.Error("default container mounts the theme")
	}
}

func TestThemeRendered(t *testing.T) {
	const doc = `theme:
  light: {page: "#ffffff"}
  dark: {bad: "#f28b82"}
  brandBar: ["#111111", "#222222", "#333333", "#444444", "#555555"]
`
	objs := render(t, "-f", themeValues(t, doc))
	cm := one(t, objs, "ConfigMap")
	data, _, _ := unstructured.NestedString(cm.Object, "data", "theme.yaml")
	th, err := theme.Parse([]byte(data))
	if err != nil || th.Light["page"] != "#ffffff" || th.Dark["bad"] != "#f28b82" || len(th.BrandBar) != 5 {
		t.Fatalf("ConfigMap theme.yaml does not load: %+v %v\n%s", th, err, data)
	}
	if !slices.Contains(args(t, objs), "--theme-file=/etc/pou/theme/theme.yaml") {
		t.Errorf("args lack --theme-file: %v", args(t, objs))
	}
	if !strings.Contains(fmt.Sprint(container(t, objs)["volumeMounts"]), "/etc/pou/theme") {
		t.Error("container does not mount the theme")
	}
	dep := one(t, objs, "Deployment")
	vols, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "volumes")
	if !strings.Contains(fmt.Sprint(vols), cm.GetName()) {
		t.Errorf("no volume uses ConfigMap %s: %v", cm.GetName(), vols)
	}
	sum := func(objs []*unstructured.Unstructured) string {
		s, _, _ := unstructured.NestedString(one(t, objs, "Deployment").Object,
			"spec", "template", "metadata", "annotations", "checksum/theme")
		return s
	}
	first := sum(objs)
	changed := sum(render(t, "-f", themeValues(t, strings.Replace(doc, "#ffffff", "#fefefe", 1))))
	if first == "" || first == changed {
		t.Errorf("checksum/theme %q then %q: a color change must restart the pod", first, changed)
	}
}

func TestThemeSchemaRejects(t *testing.T) {
	for name, doc := range map[string]string{
		"unknown key":     "theme:\n  dark: {bda: \"#fff\"}\n",
		"named color":     "theme:\n  light: {page: red}\n",
		"unquoted hex":    "theme:\n  light:\n    page: #fff\n",
		"short brand bar": "theme:\n  brandBar: [\"#111\", \"#222\", \"#333\", \"#444\"]\n",
		"unknown section": "theme:\n  colors: {page: \"#fff\"}\n",
	} {
		if out, err := helmTemplate(t, "-f", themeValues(t, doc)); err == nil {
			t.Errorf("%s: helm template accepted it:\n%.300s", name, out)
		}
	}
}

func TestS3RegionAndAmbientCredentials(t *testing.T) {
	objs := render(t, "--set", "s3History.enabled=true", "--set", "s3History.region=eu-north-1",
		"--set", "s3History.ambientCredentials=true",
		"--set", "serviceAccount.annotations.eks\\.amazonaws\\.com/role-arn=arn:aws:iam::123456789012:role/pou")
	env := fmt.Sprint(container(t, objs)["env"])
	if !strings.Contains(env, "AWS_REGION") || strings.Contains(env, "AWS_ACCESS_KEY_ID") {
		t.Errorf("env %s, want AWS_REGION and no static keys", env)
	}
	sa := one(t, objs, "ServiceAccount")
	if sa.GetAnnotations()["eks.amazonaws.com/role-arn"] == "" {
		t.Error("service account annotations not rendered")
	}
}

func TestS3SchemaChecksCredentialsSecret(t *testing.T) {
	if out, err := helmTemplate(t, "--set", "s3History.credentialsSecret.nam=x"); err == nil {
		t.Errorf("a misspelled credentialsSecret key was accepted:\n%.300s", out)
	}
}

func TestLocalLogout(t *testing.T) {
	if a := args(t, render(t)); slices.Contains(a, "--oidc.local-logout=true") {
		t.Errorf("default args keep logout local: %v", a)
	}
	if a := args(t, render(t, "--set", "oidc.localLogout=true")); !slices.Contains(a, "--oidc.local-logout=true") {
		t.Errorf("args lack --oidc.local-logout=true: %v", a)
	}
}

// The install commands in the docs must name the registry path releases are pushed to.
func TestDocsNameThePublishedChart(t *testing.T) {
	mise, err := os.ReadFile("../../mise.toml")
	if err != nil {
		t.Fatal(err)
	}
	const published = "oci://ghcr.io/jalet/helm-charts"
	if !strings.Contains(string(mise), published) {
		t.Fatalf("mise.toml no longer pushes to %s", published)
	}
	for _, doc := range []string{"../../README.md", "../../charts/pulumi-operator-ui/README.md"} {
		b, err := os.ReadFile(doc)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), published+"/pulumi-operator-ui") {
			t.Errorf("%s does not install from %s", doc, published)
		}
	}
}
