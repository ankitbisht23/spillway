// Package kind holds static tests of the kind cluster's files (ADR-0075
// §D10): deploy/kind's cluster config and manifests, and the scripts and
// Makefile targets that drive them. make kind-smoke checks a running
// cluster; these run with go test, without Docker.
package kind

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// repo is the repository root.
var repo = os.DirFS("../..")

// nodeImage is kind v0.33.0's Kubernetes 1.37.0 node image, as its release
// notes give it.
const nodeImage = "kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5"

// newImages are the images deploy/kind may use that compose does not (yet)
// use, each with the ADR that adds it.
var newImages = map[string]string{
	"oliver006/redis_exporter:v1.97.0": "ADR-0075 §D10",
}

// docs decodes every YAML document of the named file.
func docs(t *testing.T, name string) []map[string]any {
	t.Helper()
	b, err := fs.ReadFile(repo, name)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	dec := yaml.NewDecoder(bytes.NewReader(b))
	for {
		var d map[string]any
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if d != nil {
			out = append(out, d)
		}
	}
}

// manifests are every Kubernetes manifest kind-up and kind-smoke apply.
func manifests(t *testing.T) map[string][]map[string]any {
	t.Helper()
	files := []string{"deploy/kind/namespaces.yaml", "test/kind/smoke-job.yaml"}
	for _, dir := range []string{"deploy/kind/deps", "deploy/kind/obs"} {
		m, err := fs.Glob(repo, path.Join(dir, "*.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	out := map[string][]map[string]any{}
	for _, f := range files {
		out[f] = docs(t, f)
	}
	return out
}

func get(v any, path ...string) any {
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

func list(v any) []any { l, _ := v.([]any); return l }

// podSpec returns the pod spec of a workload manifest, or nil.
func podSpec(d map[string]any) map[string]any {
	switch d["kind"] {
	case "Deployment", "StatefulSet", "Job":
		s, _ := get(d, "spec", "template", "spec").(map[string]any)
		return s
	}
	return nil
}

func containers(s map[string]any) []map[string]any {
	var out []map[string]any
	for _, k := range []string{"initContainers", "containers"} {
		for _, c := range list(s[k]) {
			if m, ok := c.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out
}

func TestClusterConfig(t *testing.T) {
	d := docs(t, "deploy/kind/cluster.yaml")
	if len(d) != 1 {
		t.Fatalf("%d documents, want 1", len(d))
	}
	c := d[0]
	if c["kind"] != "Cluster" || c["name"] != "spillway" {
		t.Errorf("kind %v name %v, want Cluster spillway", c["kind"], c["name"])
	}
	if get(c, "networking", "podSubnet") != "10.244.0.0/16" || get(c, "networking", "serviceSubnet") != "10.96.0.0/16" {
		t.Errorf("networking %v, want kind's default subnets written out", c["networking"])
	}
	var roles []string
	mappings := 0
	for _, n := range list(c["nodes"]) {
		roles = append(roles, get(n, "role").(string))
		if img := get(n, "image"); img != nodeImage {
			t.Errorf("node image %v, want %s", img, nodeImage)
		}
		for _, m := range list(get(n, "extraPortMappings")) {
			mappings++
			if get(n, "role") != "control-plane" {
				t.Errorf("a %v node maps a port", get(n, "role"))
			}
			want := map[string]any{"containerPort": 30443, "hostPort": 443, "listenAddress": "127.0.0.1", "protocol": "TCP"}
			for k, v := range want {
				if get(m, k) != v {
					t.Errorf("extraPortMappings %s = %v, want %v", k, get(m, k), v)
				}
			}
		}
	}
	if !slices.Equal(roles, []string{"control-plane", "worker", "worker"}) {
		t.Errorf("nodes %v, want one control plane and two workers", roles)
	}
	if mappings != 1 {
		t.Errorf("%d port mappings, want only 127.0.0.1:443 -> 30443", mappings)
	}
	ports, err := fs.ReadFile(repo, "docs/ports.md")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(ports, []byte("| kind gateway edge (M7) | 443 |")) || !bytes.Contains(ports, []byte("**30443**")) {
		t.Error("docs/ports.md does not name the kind edge 443 and the NodePort 30443")
	}
}

// composeImages are the third-party images compose runs: its image: lines
// and the bases of its deploy/compose Dockerfiles (busybox, which those
// add for healthchecks, aside).
func composeImages(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	b, err := fs.ReadFile(repo, "compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`(?m)^\s*image:\s*(\S+)`).FindAllSubmatch(b, -1) {
		if img := string(m[1]); !strings.HasPrefix(img, "spillway-") {
			out[img] = true
		}
	}
	files, err := fs.Glob(repo, "deploy/compose/*/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		b, err := fs.ReadFile(repo, f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range regexp.MustCompile(`(?m)^FROM\s+(\S+)`).FindAllSubmatch(b, -1) {
			if img := string(m[1]); !strings.HasPrefix(img, "busybox:") {
				out[img] = true
			}
		}
	}
	return out
}

// TestManifestImages: deploy/kind runs compose's pinned images (the box's
// "same pinned images as compose"), the ADR-listed new ones, and the local
// spillway-<name>:kind images, which only kind load provides.
func TestManifestImages(t *testing.T) {
	compose := composeImages(t)
	if len(compose) < 8 {
		t.Fatalf("found only %d compose images: %v", len(compose), compose)
	}
	local := regexp.MustCompile(`^spillway-(gateway|controlplane|loadgen|chaos):kind$`)
	for f, ds := range manifests(t) {
		for _, d := range ds {
			for _, c := range containers(podSpec(d)) {
				img, _ := c["image"].(string)
				switch {
				case compose[img]:
				case newImages[img] != "":
				case local.MatchString(img):
					if c["imagePullPolicy"] != "Never" {
						t.Errorf("%s: %s needs imagePullPolicy: Never", f, img)
					}
				default:
					t.Errorf("%s: image %q is neither compose's nor listed as new", f, img)
				}
			}
		}
	}
}

// TestManifestShape: no Secret objects (they are made from the dev files by
// reference); every workload in one of the three namespaces; every
// container with a memory limit and a request of half of it (the memory
// plan, ADR-0075 §D10).
func TestManifestShape(t *testing.T) {
	namespaces := []string{"spillway", "spillway-deps", "spillway-obs"}
	var limitMi int
	for f, ds := range manifests(t) {
		for _, d := range ds {
			kind, _ := d["kind"].(string)
			name, _ := get(d, "metadata", "name").(string)
			if kind == "Secret" {
				t.Errorf("%s: Secret %s in a manifest", f, name)
			}
			ns, _ := get(d, "metadata", "namespace").(string)
			if kind != "Namespace" && !slices.Contains(namespaces, ns) {
				t.Errorf("%s: %s %s outside the three namespaces", f, kind, name)
			}
			s := podSpec(d)
			if s == nil {
				continue
			}
			for _, c := range containers(s) {
				lim, _ := get(c, "resources", "limits", "memory").(string)
				req, _ := get(c, "resources", "requests", "memory").(string)
				l, r := mebibytes(lim), mebibytes(req)
				if l == 0 || r*2 != l {
					t.Errorf("%s: %s/%s memory request %q, limit %q; want a limit and half of it requested", f, name, c["name"], req, lim)
				}
				if kind != "Job" {
					n := 1
					if rep, ok := get(d, "spec", "replicas").(int); ok {
						n = rep
					}
					limitMi += n * l
				}
			}
		}
	}
	// Dependencies and observability, without kind's own ~1.2 GB and the
	// chart: ADR-0075's plan gives them about 3.3 GB of the 5.5.
	if limitMi > 3500 {
		t.Errorf("long-running limits add up to %d Mi, over the memory plan", limitMi)
	}
}

func mebibytes(q string) int {
	var n int
	switch {
	case strings.HasSuffix(q, "Mi"):
		n = atoi(strings.TrimSuffix(q, "Mi"))
	case strings.HasSuffix(q, "Gi"):
		n = 1024 * atoi(strings.TrimSuffix(q, "Gi"))
	}
	return n
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = 10*n + int(r-'0')
	}
	return n
}

// TestContextDiscipline: every kubectl the kind scripts and Makefile
// targets run passes --context kind-spillway, every helm --kube-context
// kind-spillway, and kind gets --name spillway (ADR-0075 §D10, CLAUDE.md).
func TestContextDiscipline(t *testing.T) {
	type rule struct {
		re   *regexp.Regexp
		want string
	}
	rules := []rule{
		{regexp.MustCompile(`(\$BIN|\$\(BIN\))/kubectl"?\s`), "--context kind-spillway"},
		{regexp.MustCompile(`(\$BIN|\$\(BIN\))/helm"?\s`), "--kube-context kind-spillway"},
		{regexp.MustCompile(`\bkind (create|delete|load|export|get kubeconfig)\b`), "--name"},
	}
	files := []string{"Makefile", "deploy/kind/up.sh", "test/kind/smoke.sh"}
	seen := 0
	for _, f := range files {
		fh, err := repo.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(fh)
		for n := 1; sc.Scan(); n++ {
			line := sc.Text()
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			for _, r := range rules {
				if r.re.MatchString(line) {
					seen++
					if !strings.Contains(line, r.want) {
						t.Errorf("%s:%d lacks %s: %s", f, n, r.want, line)
					}
				}
			}
		}
		_ = fh.Close()
	}
	if seen < 4 {
		t.Errorf("only %d kubectl/helm/kind lines found; the patterns are stale", seen)
	}
	// The scripts call kubectl through a function that carries the context.
	for _, f := range []string{"deploy/kind/up.sh", "test/kind/smoke.sh"} {
		b, err := fs.ReadFile(repo, f)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(b, []byte(`kubectl() { "$BIN/kubectl" --context kind-spillway`)) {
			t.Errorf("%s: kubectl is not the function that passes --context kind-spillway", f)
		}
	}
}
