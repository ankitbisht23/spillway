package kind

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// Static tests of the Helm chart `spillway` (ADR-0075 §D11, ADR-0086),
// rendered with the pinned helm. make lint-helm sets SPILLWAY_HELM to
// bin/helm, so there the tests never skip; under a bare go test they use
// bin/helm when make tools installed it, and skip otherwise.

const chart = "../../deploy/helm/spillway"

func helmBinary(t *testing.T) string {
	t.Helper()
	if h := os.Getenv("SPILLWAY_HELM"); h != "" {
		return h
	}
	h, err := filepath.Abs("../../bin/helm")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h); err != nil {
		t.Skip("bin/helm is not installed (make tool-helm); make lint-helm runs these tests")
	}
	return h
}

// render runs helm template with the kind values and extra arguments and
// returns the documents, or the error output.
func render(t *testing.T, kindValues bool, extra ...string) ([]map[string]any, string, error) {
	t.Helper()
	args := []string{"--kube-context", "kind-spillway", "template", "spillway", chart, "-n", "spillway"}
	if kindValues {
		args = append(args, "-f", "../../deploy/kind/values.yaml", "--set", "networkPolicy.kubeletCIDRs={172.18.0.0/16}")
	}
	args = append(args, extra...)
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(t.Context(), helmBinary(t), args...) //nolint:gosec // G204: the pinned helm with fixed arguments
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, stderr.String(), err
	}
	var out []map[string]any
	dec := yaml.NewDecoder(&stdout)
	for {
		var d map[string]any
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			return out, "", nil
		}
		if err != nil {
			t.Fatalf("rendered YAML: %v", err)
		}
		if d != nil {
			out = append(out, d)
		}
	}
}

func mustRender(t *testing.T, extra ...string) []map[string]any {
	t.Helper()
	ds, stderr, err := render(t, true, extra...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, stderr)
	}
	return ds
}

func find(t *testing.T, ds []map[string]any, kind, name string) map[string]any {
	t.Helper()
	for _, d := range ds {
		if d["kind"] == kind && get(d, "metadata", "name") == name {
			return d
		}
	}
	t.Fatalf("no %s %s rendered", kind, name)
	return nil
}

// envOf returns a container's env as name -> value, a valueFrom as
// "from:<field path>".
func envOf(c map[string]any) map[string]string {
	out := map[string]string{}
	for _, e := range list(c["env"]) {
		name, _ := get(e, "name").(string)
		if v, ok := get(e, "value").(string); ok {
			out[name] = v
		} else if p, ok := get(e, "valueFrom", "fieldRef", "fieldPath").(string); ok {
			out[name] = "from:" + p
		} else {
			out[name] = "from:other"
		}
	}
	return out
}

// TestChartPodSecurity: every pod the chart renders is hardened (SEC-10,
// ADR-0075 §D11): non-root 65532, the runtime's seccomp profile, no
// service-account token, no privilege escalation, a read-only root
// filesystem, every capability dropped, a memory limit with half of it
// requested; secrets reach containers only as files (SEC-09) and the chart
// renders no Secret.
func TestChartPodSecurity(t *testing.T) {
	ds := mustRender(t, "--set", "gateway.snapshotSeed.secretName=spillway-gateway-seed")
	pods := 0
	for _, d := range ds {
		name, _ := get(d, "metadata", "name").(string)
		if d["kind"] == "Secret" {
			t.Errorf("Secret %s rendered", name)
		}
		if d["kind"] == "ServiceAccount" && d["automountServiceAccountToken"] != false {
			t.Errorf("ServiceAccount %s mounts a token", name)
		}
		if d["kind"] == "Role" || d["kind"] == "ClusterRole" || d["kind"] == "RoleBinding" || d["kind"] == "ClusterRoleBinding" {
			t.Errorf("%s %s rendered: nothing calls the Kubernetes API", d["kind"], name)
		}
		s := podSpec(d)
		if s == nil {
			continue
		}
		pods++
		if s["automountServiceAccountToken"] != false || s["serviceAccountName"] != name {
			t.Errorf("%s: service account %v, token %v", name, s["serviceAccountName"], s["automountServiceAccountToken"])
		}
		sc := s["securityContext"]
		if get(sc, "runAsNonRoot") != true || get(sc, "runAsUser") != 65532 || get(sc, "runAsGroup") != 65532 || get(sc, "seccompProfile", "type") != "RuntimeDefault" {
			t.Errorf("%s: pod securityContext %v", name, sc)
		}
		cs := containers(s)
		if len(cs) == 0 {
			t.Errorf("%s: no containers", name)
		}
		for _, c := range cs {
			csc := c["securityContext"]
			if get(csc, "allowPrivilegeEscalation") != false || get(csc, "readOnlyRootFilesystem") != true || !slices.Equal(list(get(csc, "capabilities", "drop")), []any{"ALL"}) {
				t.Errorf("%s/%s: container securityContext %v", name, c["name"], csc)
			}
			lim, _ := get(c, "resources", "limits", "memory").(string)
			req, _ := get(c, "resources", "requests", "memory").(string)
			if l, r := mebibytes(lim), mebibytes(req); l == 0 || r*2 != l {
				t.Errorf("%s/%s: memory request %q, limit %q", name, c["name"], req, lim)
			}
			for k, v := range envOf(c) {
				if (strings.Contains(k, "PASSWORD") || strings.Contains(k, "SECRET")) && !strings.HasSuffix(k, "_FILE") {
					t.Errorf("%s/%s: %s=%q: secrets come as files only (SEC-09)", name, c["name"], k, v)
				}
			}
		}
	}
	if pods != 3 {
		t.Errorf("%d pod templates, want gateway, control plane and migrate Job", pods)
	}
}

// TestChartGateway: the gateway is the edge with TLS only, surges without
// becoming unavailable, sleeps before SIGTERM, probes and binds admin on the
// pod IP, names itself by its pod, keeps its snapshot on a 64 Mi emptyDir
// and allows kind's ranges (GW-25, SEC-01, SEC-06, CP-07, NFR-14).
func TestChartGateway(t *testing.T) {
	ds := mustRender(t)
	d := find(t, ds, "Deployment", "spillway-gateway")
	if get(d, "spec", "replicas") != 3 || get(d, "spec", "strategy", "rollingUpdate", "maxSurge") != 1 || get(d, "spec", "strategy", "rollingUpdate", "maxUnavailable") != 0 {
		t.Errorf("replicas / strategy: %v %v", get(d, "spec", "replicas"), get(d, "spec", "strategy"))
	}
	s := podSpec(d)
	if s["terminationGracePeriodSeconds"] != 45 || s["initContainers"] != nil {
		t.Errorf("grace %v, init containers %v", s["terminationGracePeriodSeconds"], s["initContainers"])
	}
	c := containers(s)[0]
	if get(c, "lifecycle", "preStop", "sleep", "seconds") != 10 {
		t.Errorf("preStop %v", c["lifecycle"])
	}
	var ports []string
	for _, p := range list(c["ports"]) {
		ports = append(ports, get(p, "name").(string))
	}
	if !slices.Equal(ports, []string{"https", "admin"}) {
		t.Errorf("container ports %v", ports)
	}
	if get(c, "readinessProbe", "httpGet", "path") != "/readyz" || get(c, "readinessProbe", "httpGet", "port") != "admin" || get(c, "readinessProbe", "failureThreshold") != 1 ||
		get(c, "livenessProbe", "httpGet", "path") != "/healthz" || get(c, "livenessProbe", "httpGet", "port") != "admin" {
		t.Errorf("probes %v %v", c["readinessProbe"], c["livenessProbe"])
	}
	env := envOf(c)
	want := map[string]string{ //nolint:gosec // G101: setting names and file paths, not credentials
		"POD_IP":                                    "from:status.podIP",
		"SPILLWAY_GATEWAY_REPLICA_ID":               "from:metadata.name",
		"SPILLWAY_GATEWAY_ADMIN_ADDR":               "$(POD_IP):8001",
		"SPILLWAY_GATEWAY_TLS_ADDR":                 ":8443",
		"SPILLWAY_GATEWAY_CONTROL_PLANE":            "spillway-controlplane.spillway.svc:9090",
		"SPILLWAY_GATEWAY_SNAPSHOT":                 "/var/lib/spillway/snapshot.pb",
		"SPILLWAY_GATEWAY_ALLOWED_NETWORKS":         "10.244.0.0/16,10.96.0.0/16",
		"SPILLWAY_GATEWAY_JWT_ISSUER":               "https://keycloak.spillway-deps.svc.cluster.local:8443/auth/realms/spillway",
		"SPILLWAY_GATEWAY_JWT_CA":                   "/etc/spillway-ca/ca.pem",
		"SPILLWAY_GATEWAY_MAX_CONNECTIONS_PER_IP":   "1024",
		"SPILLWAY_GATEWAY_HEAVY_HITTER_SECRET_FILE": "/etc/spillway-secrets/heavy-hitter/value",
		"SPILLWAY_GATEWAY_REDIS_PASSWORD_FILE":      "/etc/spillway-secrets/redis/value",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	for _, k := range []string{"SPILLWAY_GATEWAY_HTTP_ADDR", "SPILLWAY_GATEWAY_DEV_MODE", "SPILLWAY_GATEWAY_CONFIG", "SPILLWAY_REDIS_ADDR_MAP"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s is set: no plain HTTP, dev mode, static config or address map in Helm", k)
		}
	}
	state := false
	for _, v := range list(s["volumes"]) {
		if get(v, "name") == "state" {
			state = get(v, "emptyDir", "sizeLimit") == "64Mi"
		}
	}
	if !state {
		t.Errorf("no 64Mi emptyDir state volume: %v", s["volumes"])
	}

	svc := find(t, ds, "Service", "spillway-gateway")
	sp := list(get(svc, "spec", "ports"))
	if get(svc, "spec", "type") != "NodePort" || get(svc, "spec", "externalTrafficPolicy") != "Cluster" || len(sp) != 1 ||
		get(sp[0], "port") != 443 || get(sp[0], "nodePort") != 30443 || get(sp[0], "targetPort") != "https" {
		t.Errorf("edge Service %v", svc["spec"])
	}
	// No Service reaches an admin port.
	for _, d := range ds {
		if d["kind"] != "Service" {
			continue
		}
		for _, p := range list(get(d, "spec", "ports")) {
			if tp := get(p, "targetPort"); tp == "admin" || tp == 8001 || tp == 8081 || get(p, "port") == 80 {
				t.Errorf("Service %v exposes %v", get(d, "metadata", "name"), p)
			}
		}
	}
	pdb := find(t, ds, "PodDisruptionBudget", "spillway-gateway")
	if get(pdb, "spec", "maxUnavailable") != 1 {
		t.Errorf("PDB %v", pdb["spec"])
	}
}

// TestChartSnapshotSeed: with gateway.snapshotSeed.secretName set, an init
// container seeds the snapshot volume from the optional Secret (CP-07).
func TestChartSnapshotSeed(t *testing.T) {
	ds := mustRender(t, "--set", "gateway.snapshotSeed.secretName=spillway-gateway-seed")
	s := podSpec(find(t, ds, "Deployment", "spillway-gateway"))
	ic := list(s["initContainers"])
	if len(ic) != 1 || !slices.Equal(list(get(ic[0], "args")), []any{"snapshot-seed", "--from", "/etc/spillway-seed/snapshot.pb", "--to", "/var/lib/spillway/snapshot.pb"}) {
		t.Fatalf("init containers %v", ic)
	}
	for _, v := range list(s["volumes"]) {
		if get(v, "name") == "seed" {
			if get(v, "secret", "secretName") != "spillway-gateway-seed" || get(v, "secret", "optional") != true {
				t.Errorf("seed volume %v", v)
			}
			return
		}
	}
	t.Error("no seed volume")
}

// TestChartControlPlane: one replica (CP-22), surge 1 and unavailable 0,
// admin on the pod IP, the fleet identity, OIDC over https with the CA,
// the quota flusher's Redis; replicas other than 1 do not render.
func TestChartControlPlane(t *testing.T) {
	ds := mustRender(t)
	d := find(t, ds, "Deployment", "spillway-controlplane")
	if get(d, "spec", "replicas") != 1 || get(d, "spec", "strategy", "rollingUpdate", "maxUnavailable") != 0 {
		t.Errorf("replicas %v strategy %v", get(d, "spec", "replicas"), get(d, "spec", "strategy"))
	}
	env := envOf(containers(podSpec(d))[0])
	for k, v := range map[string]string{ //nolint:gosec // G101: setting names and file paths, not credentials
		"SPILLWAY_CP_ADMIN_ADDR":             "$(POD_IP):8081",
		"SPILLWAY_CP_GRPC_FLEET_IDENTITIES":  "spillway-gateway",
		"SPILLWAY_CP_OIDC_CA":                "/etc/spillway-ca/ca.pem",
		"SPILLWAY_CP_ALLOWED_NETWORKS":       "10.244.0.0/16,10.96.0.0/16",
		"SPILLWAY_CP_DATABASE_PASSWORD_FILE": "/etc/spillway-secrets/database/value",
		"SPILLWAY_CP_REDIS_PASSWORD_FILE":    "/etc/spillway-secrets/redis/value",
	} {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	if !strings.HasPrefix(env["SPILLWAY_CP_OIDC_ISSUER"], "https://") || !strings.HasPrefix(env["SPILLWAY_CP_OIDC_JWKS_URL"], "https://") || env["SPILLWAY_CP_REDIS_ADDRS"] == "" {
		t.Errorf("OIDC %q %q, Redis %q", env["SPILLWAY_CP_OIDC_ISSUER"], env["SPILLWAY_CP_OIDC_JWKS_URL"], env["SPILLWAY_CP_REDIS_ADDRS"])
	}
	if _, ok := env["SPILLWAY_CP_DEV_MODE"]; ok {
		t.Error("SPILLWAY_CP_DEV_MODE is set (SEC-04)")
	}
	svc := find(t, ds, "Service", "spillway-controlplane")
	if get(svc, "spec", "type") != "ClusterIP" {
		t.Errorf("control-plane Service type %v: internal only (SEC-02)", get(svc, "spec", "type"))
	}
	for _, n := range []string{"0", "2"} {
		if _, stderr, err := render(t, true, "--set", "controlplane.replicas="+n); err == nil || !strings.Contains(stderr, "controlplane.replicas must be 1") {
			t.Errorf("replicas %s: %v %s", n, err, stderr)
		}
	}
}

// TestChartMigrate: the migrate Job is a pre-install/pre-upgrade hook with
// a deadline, after its own hook service account (CP-19).
func TestChartMigrate(t *testing.T) {
	ds := mustRender(t)
	j := find(t, ds, "Job", "spillway-migrate")
	if get(j, "metadata", "annotations", "helm.sh/hook") != "pre-install,pre-upgrade" || get(j, "metadata", "annotations", "helm.sh/hook-delete-policy") != "before-hook-creation,hook-succeeded" ||
		get(j, "spec", "activeDeadlineSeconds") != 300 {
		t.Errorf("Job %v %v", get(j, "metadata", "annotations"), j["spec"])
	}
	if !slices.Equal(list(get(containers(podSpec(j))[0], "args")), []any{"migrate"}) {
		t.Errorf("args %v", get(containers(podSpec(j))[0], "args"))
	}
	sa := find(t, ds, "ServiceAccount", "spillway-migrate")
	if get(sa, "metadata", "annotations", "helm.sh/hook-weight") != "-10" {
		t.Errorf("migrate service account %v", get(sa, "metadata", "annotations"))
	}
}

// TestChartNetworkPolicies: GW-25: admin ports admit only Prometheus (in
// its namespace) and the node network; gateway traffic is open; the
// control plane's gRPC admits the gateways and its REST this namespace.
func TestChartNetworkPolicies(t *testing.T) {
	ds := mustRender(t)
	prom := map[string]any{
		"namespaceSelector": map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": "spillway-obs"}},
		"podSelector":       map[string]any{"matchLabels": map[string]any{"app.kubernetes.io/name": "prometheus"}},
	}
	node := map[string]any{"ipBlock": map[string]any{"cidr": "172.18.0.0/16"}}
	rules := func(name string) map[int][]any {
		np := find(t, ds, "NetworkPolicy", name)
		if !slices.Equal(list(get(np, "spec", "policyTypes")), []any{"Ingress"}) || get(np, "spec", "podSelector", "matchLabels", "app.kubernetes.io/name") != name {
			t.Errorf("%s: %v", name, np["spec"])
		}
		out := map[int][]any{}
		for _, r := range list(get(np, "spec", "ingress")) {
			ps := list(get(r, "ports"))
			if len(ps) != 1 {
				t.Fatalf("%s: rule %v has %d ports", name, r, len(ps))
			}
			from := list(get(r, "from"))
			if from == nil {
				from = []any{"anywhere"}
			}
			out[get(ps[0], "port").(int)] = from
		}
		return out
	}
	eq := func(a, b []any) bool { x, _ := yaml.Marshal(a); y, _ := yaml.Marshal(b); return bytes.Equal(x, y) }
	gw := rules("spillway-gateway")
	if len(gw) != 2 || !eq(gw[8443], []any{"anywhere"}) || !eq(gw[8001], []any{prom, node}) {
		t.Errorf("gateway rules %v", gw)
	}
	cp := rules("spillway-controlplane")
	gwPods := map[string]any{"podSelector": map[string]any{"matchLabels": map[string]any{"app.kubernetes.io/name": "spillway-gateway", "app.kubernetes.io/instance": "spillway"}}}
	if len(cp) != 3 || !eq(cp[9090], []any{gwPods}) || !eq(cp[8080], []any{map[string]any{"podSelector": map[string]any{}}}) || !eq(cp[8081], []any{prom, node}) {
		t.Errorf("control-plane rules %v", cp)
	}
}

// TestChartRequiredValues: the chart's defaults are not silently usable:
// without the kind values (or a deployment's own) rendering fails, and
// without kubeletCIDRs the NetworkPolicies would drop the probes.
func TestChartRequiredValues(t *testing.T) {
	if _, stderr, err := render(t, false); err == nil || !strings.Contains(stderr, "execution error") {
		t.Errorf("defaults rendered: %v %s", err, stderr)
	}
	_, stderr, err := render(t, false, "-f", "../../deploy/kind/values.yaml")
	if err == nil || !strings.Contains(stderr, "networkPolicy.kubeletCIDRs") {
		t.Errorf("no kubeletCIDRs: %v %s", err, stderr)
	}
	// The pod names (the replica IDs) must be replicas of the fleet.
	for _, f := range []string{"gateway-fleet", "spillway-gateway-x", ""} {
		if _, stderr, err := render(t, true, "--set", "gateway.stream.fleetName="+f); err == nil || !strings.Contains(stderr, "gateway.stream.fleetName") {
			t.Errorf("fleetName %q rendered: %v %s", f, err, stderr)
		}
	}
	mustRender(t, "--set", "gateway.stream.fleetName=spillway")
}
