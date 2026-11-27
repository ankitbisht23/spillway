// Package deploy holds static tests of the files that start containers:
// the compose files, the Helm chart, the kind cluster's files, the
// Dockerfiles, the demo's scripts and the workflows. They run with go test,
// without Docker.
package deploy

import (
	"io/fs"
	"os"
	"path"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// repo is the repository root.
var repo = os.DirFS("../..")

// composeFiles are every compose file of the repository.
var composeFiles = []string{"compose.yaml", "deploy/compose/demo.compose.yml", "test/chaos/latency.compose.yml"}

// containerRoots are the files and directories that define containers; the
// key directories deploy/certs and deploy/stream-certs are not among them,
// so no private key is read.
var containerRoots = []string{
	"compose.yaml", "deploy/compose", "deploy/demo", "deploy/helm", "deploy/kind", "test/chaos",
	"Dockerfile", ".github/workflows",
}

// sockets are the container-runtime sockets no container may get.
var sockets = []string{"docker.sock", "containerd.sock", "podman.sock"}

// socketMount reports whether a compose volume, in its short or long form,
// mounts a container-runtime socket or the directory that holds it.
func socketMount(v any) bool {
	var src string
	switch v := v.(type) {
	case string:
		src, _, _ = strings.Cut(v, ":")
	case map[string]any:
		src, _ = v["source"].(string)
	}
	src = path.Clean(src)
	if src == "/var/run" || src == "/run" || strings.HasPrefix(src, "/var/run/docker") || strings.HasPrefix(src, "/run/docker") {
		return true
	}
	for _, s := range sockets {
		if strings.Contains(src, s) {
			return true
		}
	}
	return false
}

// TestNoDockerSocket: no compose service mounts the Docker socket (or the
// directory that holds it), and no compose file, chart template, kind
// manifest, Dockerfile, demo script or workflow names a container-runtime
// socket, so no container can drive Docker: the demo's fault switches go
// through toxiproxy's API only (SC-03, ADR-0095 §5).
func TestNoDockerSocket(t *testing.T) {
	services := 0
	for _, name := range composeFiles {
		b, err := fs.ReadFile(repo, name)
		if err != nil {
			t.Fatal(err)
		}
		var f struct {
			Services map[string]struct {
				Volumes []any `yaml:"volumes"`
			} `yaml:"services"`
		}
		if err := yaml.Unmarshal(b, &f); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for svc, s := range f.Services {
			services++
			for _, v := range s.Volumes {
				if socketMount(v) {
					t.Errorf("%s: service %s mounts %v", name, svc, v)
				}
			}
		}
	}
	if services < 30 {
		t.Fatalf("only %d compose services read", services)
	}
	files := 0
	for _, root := range containerRoots {
		err := fs.WalkDir(repo, root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if ext := path.Ext(p); ext == ".pem" || ext == ".key" {
				return nil // never read a key
			}
			files++
			b, err := fs.ReadFile(repo, p)
			if err != nil {
				return err
			}
			for _, s := range sockets {
				if strings.Contains(string(b), s) {
					t.Errorf("%s names %s", p, s)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if files < 50 {
		t.Fatalf("only %d files read", files)
	}
}

// TestSocketMount: the rule finds a socket mount in each form.
func TestSocketMount(t *testing.T) {
	for v, want := range map[any]bool{
		"/var/run/docker.sock:/var/run/docker.sock":   true,
		"/var/run/docker.sock:/sock:ro":               true,
		"/var/run:/host-run":                          true,
		"/run/containerd/containerd.sock:/c.sock":     true,
		"${HOME}/.docker/run/docker.sock:/d.sock":     true,
		"./deploy/compose/gateway:/etc/spillway:ro":   false,
		"gateway-1-state:/var/lib/spillway":           false,
		"./deploy/certs:/etc/spillway-certs:ro":       false,
		"/var/lib/docker/volumes/x:/x":                false,
		"./deploy/demo:/etc/spillway-demo:ro":         false,
		"/var/run/docker/netns:/netns":                true,
		"/run/docker.sock:/run/docker.sock":           true,
		"/var/run/./docker.sock:/var/run/docker.sock": true,
	} {
		if got := socketMount(v); got != want {
			t.Errorf("%v: %v, want %v", v, got, want)
		}
	}
	long := map[string]any{"type": "bind", "source": "/var/run/docker.sock", "target": "/var/run/docker.sock"}
	if !socketMount(long) {
		t.Error("the long form was not found")
	}
	if socketMount(map[string]any{"type": "volume", "source": "pgdata", "target": "/var/lib/postgresql"}) {
		t.Error("a named volume was taken for a socket")
	}
}
