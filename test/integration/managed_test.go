//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
	"github.com/ankitbisht23/spillway/internal/managed"
)

// Managed mode end to end, with host processes (CP-06, CP-07, CP-18,
// CP-22, ADR-0038): control planes on a Postgres testcontainer, managed
// gateways on loopback, and an upstream in the test. Every wait allows
// 30 s, far above the 820 ms host stalls of ADR-0034, so a stall alone
// cannot fail a check.

const waitFor = 30 * time.Second

var binaries struct {
	once sync.Once
	dir  string
	err  error
}

// binary builds the gateway and the control plane once for the package.
func binary(t *testing.T, name string) string {
	t.Helper()
	binaries.once.Do(func() {
		if binaries.dir, binaries.err = os.MkdirTemp("", "spillway-it-"); binaries.err != nil {
			return
		}
		for _, cmd := range []string{"gateway", "controlplane"} {
			out, err := exec.CommandContext(context.Background(), "go", "build", "-o", filepath.Join(binaries.dir, cmd), //nolint:gosec // G204: fixed packages of this repo
				"github.com/ankitbisht23/spillway/cmd/"+cmd).CombinedOutput()
			if err != nil {
				binaries.err = fmt.Errorf("go build %s: %v\n%s", cmd, err, out)
				return
			}
		}
	})
	if binaries.err != nil {
		t.Fatal(binaries.err)
	}
	return filepath.Join(binaries.dir, name)
}

// proc is a process of the test, stopped when the test ends.
type proc struct {
	name  string
	cmd   *exec.Cmd
	out   *syncBuffer
	admin string // http://host:port of its admin server
	done  chan struct{}
}

func start(t *testing.T, name, bin, admin string, env []string) *proc {
	t.Helper()
	p := &proc{name: name, out: &syncBuffer{}, admin: "http://" + admin, done: make(chan struct{})}
	p.cmd = exec.CommandContext(context.Background(), bin) //nolint:gosec // G204: a binary built by binary
	p.cmd.Env = env
	p.cmd.Stdout, p.cmd.Stderr = p.out, p.out
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = p.cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		p.stop(syscall.SIGTERM)
		if t.Failed() {
			t.Logf("%s output:\n%s", p.name, p.out.String())
		}
	})
	return p
}

// stop sends sig and waits for the process to exit.
func (p *proc) stop(sig syscall.Signal) {
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Signal(sig)
	select {
	case <-p.done:
	case <-time.After(waitFor):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

// status is the answer to GET url, 0 when it cannot connect.
func status(url string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// metric is one sample of the process's /metrics, -1 when it cannot be
// read.
func (p *proc) metric(series string) float64 {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.admin+"/metrics", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return -1
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	for _, line := range strings.Split(string(body), "\n") {
		if v, ok := strings.CutPrefix(line, series+" "); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err == nil {
				return f
			}
		}
	}
	return -1
}

// eventually waits until cond holds, failing with what after waitFor.
func eventually(t *testing.T, what string, cond func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for !cond() {
		if time.Since(start) > waitFor {
			t.Fatalf("after %v: %s", waitFor, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return time.Since(start)
}

// hostCP is a control-plane process.
type hostCP struct {
	*proc
	rest cpClient // as admin
}

// startCP runs a control plane on database db with its gRPC listener at
// grpcAddr and bearer tokens from iss; it waits until it is ready.
func startCP(t *testing.T, name, db, grpcAddr string, iss *testIssuer) *hostCP {
	t.Helper()
	httpAddr, admin := freeAddr(t), freeAddr(t)
	p := start(t, name, binary(t, "controlplane"), admin, []string{
		"SPILLWAY_CP_DATABASE_URL=" + pgURL(t, "spillway_app", db),
		"SPILLWAY_CP_DATABASE_PASSWORD=" + pgAppPassword,
		"SPILLWAY_CP_HTTP_ADDR=" + httpAddr,
		"SPILLWAY_CP_ADMIN_ADDR=" + admin,
		"SPILLWAY_CP_GRPC_ADDR=" + grpcAddr,
		"SPILLWAY_CP_ALLOWED_NETWORKS=127.0.0.0/8",
		"SPILLWAY_CP_OIDC_ISSUER=" + testIssuerURL,
		"SPILLWAY_CP_OIDC_JWKS_URL=" + iss.jwks.URL,
		"SPILLWAY_CP_DEV_MODE=true",
	})
	eventually(t, name+" ready", func() bool { return status(p.admin+"/readyz") == 200 })
	return &hostCP{proc: p, rest: cpClient{t: t, base: "http://" + httpAddr, token: iss.token(t, "admin-1", "test-admin", "admin")}}
}

// hostGW is a managed gateway process.
type hostGW struct {
	*proc
	url string
}

// startManaged runs a managed gateway named name against the control
// plane at cp, its snapshot in dir; it does not wait for readiness.
func startManaged(t *testing.T, name, cp, dir string) *hostGW {
	t.Helper()
	httpAddr, admin := freeAddr(t), freeAddr(t)
	p := start(t, name, binary(t, "gateway"), admin, []string{
		"SPILLWAY_GATEWAY_CONTROL_PLANE=" + cp,
		"SPILLWAY_GATEWAY_REPLICA_ID=" + name,
		"SPILLWAY_GATEWAY_SNAPSHOT=" + filepath.Join(dir, "snapshot.pb"),
		"SPILLWAY_GATEWAY_HTTP_ADDR=" + httpAddr,
		"SPILLWAY_GATEWAY_ADMIN_ADDR=" + admin,
		"SPILLWAY_GATEWAY_ALLOWED_NETWORKS=127.0.0.0/8",
		"SPILLWAY_GATEWAY_DRAIN=0s",
		"SPILLWAY_GATEWAY_SHUTDOWN_GRACE=1s",
	})
	return &hostGW{proc: p, url: "http://" + httpAddr}
}

func (g *hostGW) ready() bool { return status(g.admin+"/readyz") == 200 }

// waitVersion waits until the gateway runs config version v.
func (g *hostGW) waitVersion(t *testing.T, v int) time.Duration {
	t.Helper()
	return eventually(t, fmt.Sprintf("%s at config version %d (has %v)", g.name, v, g.metric("gw_config_version")),
		func() bool { return g.metric("gw_config_version") == float64(v) })
}

// upstreamAnswering serves body at every path until the test ends.
func upstreamAnswering(t *testing.T, body string) string {
	t.Helper()
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
	t.Cleanup(u.Close)
	return strings.TrimPrefix(u.URL, "http://")
}

// publishRoute sets route r to path prefix /r on an upstream at addr, plus
// plan free, and publishes it; it returns the new version.
func publishRoute(t *testing.T, c cpClient, addr, note string) int64 {
	t.Helper()
	_, ws := exportYAML(t, c, "")
	body := []byte(fmt.Sprintf("upstreams: [{name: u, endpoints: [{address: '%s'}]}]\nroutes: [{name: r, upstream: u, match: {path_prefix: /r}}]\nplans: [{name: free}]\n", addr))
	importYAML(t, c, 200, body, ws, "")
	_, tag := validate(t, c)
	var v struct{ Version int64 }
	c.must(200, "POST", "/config/publish", map[string]any{"note": note}, "If-Match", tag).decode(t, &v)
	return v.Version
}

// TestManagedGateway checks a managed gateway against a control plane
// (CP-06, CP-07, CP-20, ADR-0038): a publish and an identity change reach
// it; with the control plane stopped it keeps serving its config; a
// restart with the control plane still down serves the on-disk snapshot
// after the 5 s wait; the control plane back, the next publish arrives.
func TestManagedGateway(t *testing.T) {
	iss := newTestIssuer(t)
	db := migratedDatabase(t)
	grpcAddr, dir := freeAddr(t), t.TempDir()
	cp := startCP(t, "controlplane", db, grpcAddr, iss)
	eventually(t, "cp_active 1", func() bool { return cp.metric("cp_active") == 1 })

	gw := startManaged(t, "gateway-1", grpcAddr, dir)
	// Nothing published: identity only, not ready, every request 503.
	eventually(t, "identity version 1", func() bool { return gw.metric("gw_identity_version") == 1 })
	if gw.ready() || status(gw.url+"/r") != 503 {
		t.Fatalf("before the first publish: ready %v, /r %d", gw.ready(), status(gw.url+"/r"))
	}
	one := upstreamAnswering(t, "one")
	if v := publishRoute(t, cp.rest, one, "first"); v != 1 {
		t.Fatalf("published version %d", v)
	}
	gw.waitVersion(t, 1)
	if !gw.ready() || status(gw.url+"/r") != 200 || cp.metric("cp_watch_streams") != 1 {
		t.Fatalf("after version 1: ready %v, /r %d, %v streams", gw.ready(), status(gw.url+"/r"), cp.metric("cp_watch_streams"))
	}
	// An identity change: its own version, on the same stream (CP-20).
	cp.rest.must(201, "POST", "/tenants", map[string]any{"name": "acme", "plan_id": planID(t, cp.rest)})
	eventually(t, "identity version 2", func() bool { return gw.metric("gw_identity_version") == 2 })
	snap := filepath.Join(dir, "snapshot.pb")
	eventually(t, "the on-disk snapshot at 1 and 2", func() bool {
		f, err := managed.ReadSnapshot(snap)
		return err == nil && f.GetConfig().GetVersion() == 1 && f.GetIdentity().GetVersion() == 2 && len(f.GetIdentity().GetTenants()) == 1
	})

	// The control plane stops: the gateway serves on.
	cp.stop(syscall.SIGTERM)
	for range 20 {
		if code := status(gw.url + "/r"); code != 200 || !gw.ready() {
			t.Fatalf("with the control plane down: /r %d, ready %v", code, gw.ready())
		}
		time.Sleep(100 * time.Millisecond)
	}

	// A restart while it is down: unready for the 5 s wait, then the
	// snapshot's config and identity.
	gw.stop(syscall.SIGTERM)
	start := time.Now()
	gw = startManaged(t, "gateway-1", grpcAddr, dir)
	took := eventually(t, "ready from the snapshot", gw.ready)
	if took < 4*time.Second || gw.metric("gw_config_version") != 1 || gw.metric("gw_identity_version") != 2 || status(gw.url+"/r") != 200 {
		t.Errorf("restart without the control plane: ready after %v (want the 5 s wait), versions %v and %v, /r %d",
			time.Since(start), gw.metric("gw_config_version"), gw.metric("gw_identity_version"), status(gw.url+"/r"))
	}
	if !strings.Contains(gw.out.String(), "serving the on-disk snapshot") {
		t.Error("the gateway did not log that it serves the snapshot")
	}

	// The control plane is back: the stream replaces the snapshot.
	cp = startCP(t, "controlplane-2", db, grpcAddr, iss)
	two := upstreamAnswering(t, "two")
	publishRoute(t, cp.rest, two, "second")
	gw.waitVersion(t, 2)
	if _, body := get(t, gw.url+"/r"); body != "two" {
		t.Errorf("after version 2: %q", body)
	}
}

// planID is plan free's ID.
func planID(t *testing.T, c cpClient) int64 {
	t.Helper()
	id, _ := lookup(t, c, "/plans", "free")
	return id
}

// TestManagedGatewaySchemaMajor checks CP-18 on both paths: a published
// version of another schema major is never streamed (the gateway keeps
// its config), and an on-disk snapshot of another major is never served.
func TestManagedGatewaySchemaMajor(t *testing.T) {
	iss := newTestIssuer(t)
	db := migratedDatabase(t)
	grpcAddr, dir := freeAddr(t), t.TempDir()
	cp := startCP(t, "controlplane", db, grpcAddr, iss)
	gw := startManaged(t, "gateway-1", grpcAddr, dir)
	publishRoute(t, cp.rest, upstreamAnswering(t, "one"), "first")
	gw.waitVersion(t, 1)

	// Version 2 of schema major 2, written behind the API's back.
	owner := pgConnect(t, pgURL(t, "spillway", db), pgOwnerPassword)
	if _, err := owner.Exec(t.Context(), `INSERT INTO config_versions (version, published_at, published_by, note, kind, working_set_revision, schema_major, schema_minor, snapshot)
		SELECT 2, clock_timestamp(), 'test', 'major 2', 'publish', working_set_revision, $1, 0, '{"version": 2}' FROM config_versions WHERE version = 1`, config.SchemaMajor+1); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(t.Context(), "SELECT pg_notify('spillway_config', '2')"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the control plane refusing version 2", func() bool {
		return strings.Contains(cp.out.String(), "config version 2 has schema 2.0, which this control plane (schema 1.0) cannot read")
	})
	time.Sleep(time.Second)
	if v := gw.metric("gw_config_version"); v != 1 {
		t.Errorf("gateway at version %v, want 1", v)
	}

	// A snapshot file of major 2 with the control plane down: not served.
	cp.stop(syscall.SIGTERM)
	gw.stop(syscall.SIGTERM)
	if err := managed.WriteSnapshot(filepath.Join(dir, "snapshot.pb"), &configpb.SnapshotFile{
		SchemaVersion: &configpb.SchemaVersion{Major: config.SchemaMajor + 1},
		Config:        &configpb.Config{Version: 9}, Identity: &configpb.Identity{Version: 9}, SyncedAt: timestamppb.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	gw = startManaged(t, "gateway-1", grpcAddr, dir)
	eventually(t, "the gateway refusing the file", func() bool { return strings.Contains(gw.out.String(), "this gateway reads schema major 1 only") })
	if gw.ready() || gw.metric("gw_config_version") != 0 {
		t.Errorf("served a snapshot of another major: ready %v, version %v", gw.ready(), gw.metric("gw_config_version"))
	}
}

// TestControlPlaneFailover checks one active replica (CP-22, D6, ADR-0038)
// with two control-plane processes on one database and one gRPC address,
// as behind a Service: one is active and listens; the standby is ready,
// serves REST and does not listen; when the active one is killed, the
// standby takes the lock and the gateway follows it; a clean exit hands
// over the same way.
func TestControlPlaneFailover(t *testing.T) {
	iss := newTestIssuer(t)
	db := migratedDatabase(t)
	grpcAddr, dir := freeAddr(t), t.TempDir()
	a := startCP(t, "controlplane-a", db, grpcAddr, iss)
	eventually(t, "a active", func() bool { return a.metric("cp_active") == 1 })
	b := startCP(t, "controlplane-b", db, grpcAddr, iss)
	time.Sleep(2 * time.Second) // b tries the lock twice
	if b.metric("cp_active") != 0 || status(b.admin+"/readyz") != 200 {
		t.Fatalf("standby: cp_active %v, readyz %d", b.metric("cp_active"), status(b.admin+"/readyz"))
	}
	if strings.Contains(b.out.String(), `"server":"grpc"`) {
		t.Fatal("the standby opened the gRPC listener")
	}
	gw := startManaged(t, "gateway-1", grpcAddr, dir)
	publishRoute(t, b.rest, upstreamAnswering(t, "one"), "through the standby's REST") // its NOTIFY reaches a's LISTEN
	gw.waitVersion(t, 1)

	// The active replica is killed: Postgres frees its lock when the
	// connection drops, the standby takes it.
	start := time.Now()
	a.stop(syscall.SIGKILL)
	took := eventually(t, "b active", func() bool { return b.metric("cp_active") == 1 })
	publishRoute(t, b.rest, upstreamAnswering(t, "two"), "after a crash")
	gw.waitVersion(t, 2)
	t.Logf("killed: the standby active after %v, the gateway at the next version %v after the kill", took, time.Since(start))

	// The killed replica comes back as a standby; a clean exit of the
	// active one hands over the same way.
	a = startCP(t, "controlplane-a2", db, grpcAddr, iss)
	time.Sleep(2 * time.Second)
	if a.metric("cp_active") != 0 {
		t.Fatal("the restarted replica is active beside the active one")
	}
	start = time.Now()
	b.stop(syscall.SIGTERM)
	took = eventually(t, "a2 active", func() bool { return a.metric("cp_active") == 1 })
	publishRoute(t, a.rest, upstreamAnswering(t, "three"), "after a clean exit")
	gw.waitVersion(t, 3)
	t.Logf("clean exit: the standby active after %v, the gateway at the next version %v after the exit", took, time.Since(start))
	if took > 2*time.Second {
		t.Errorf("handover after a clean exit took %v; the standby tries every second", took)
	}
}

// TestManagedPropagation is a quick propagation measurement, not NFR-08's
// check (that is M5's verify task, from heartbeats on Postgres's clock): 20
// publishes to 5 managed host gateways, each timed from the publish request
// to the moment every gateway's gw_config_version shows it, polled every
// 10 ms on the test's clock. It fails only if a publish does not arrive
// within 30 s, and logs p50 and p99.
func TestManagedPropagation(t *testing.T) {
	iss := newTestIssuer(t)
	db := migratedDatabase(t)
	grpcAddr := freeAddr(t)
	cp := startCP(t, "controlplane", db, grpcAddr, iss)
	base := publishRoute(t, cp.rest, upstreamAnswering(t, "ok"), "base")
	var gws []*hostGW
	for i := range 5 {
		gws = append(gws, startManaged(t, fmt.Sprintf("gateway-%d", i+1), grpcAddr, t.TempDir()))
	}
	for _, g := range gws {
		g.waitVersion(t, int(base))
	}
	var took []time.Duration
	for i := range 20 {
		a := cp.rest.must(200, "GET", "/config/settings", nil)
		cp.rest.must(200, "PUT", "/config/settings", map[string]any{"ratelimit": map[string]any{"replicas": i%2 + 1}}, "If-Match", a.etag())
		_, tag := validate(t, cp.rest)
		start := time.Now()
		var v struct{ Version int64 }
		cp.rest.must(200, "POST", "/config/publish", map[string]any{"note": fmt.Sprint("publish ", i)}, "If-Match", tag).decode(t, &v)
		eventually(t, fmt.Sprintf("version %d on every gateway", v.Version), func() bool {
			for _, g := range gws {
				if g.metric("gw_config_version") != float64(v.Version) {
					return false
				}
			}
			return true
		})
		took = append(took, time.Since(start))
	}
	slices.Sort(took)
	t.Logf("publish → applied on all 5 gateways, %d publishes: p50 %v, p99 %v (nearest rank: the maximum), min %v",
		len(took), took[len(took)/2-1].Round(time.Millisecond), took[len(took)-1].Round(time.Millisecond), took[0].Round(time.Millisecond))
}

// TestComposeManagedGateways runs the compose gateways in managed mode, as
// make verify-m5 will (ADR-0038): gateway-1 … gateway-5 recreated with the
// compose control plane and no files, a publish reaches all five, and they
// go back to static mode afterwards.
func TestComposeManagedGateways(t *testing.T) {
	c := composeControlPlane(t)
	c = c.as(keycloakToken(t, "client_credentials", "spillway-ci", ""))
	root := filepath.Join("..", "..")
	compose := func(env []string, args ...string) {
		t.Helper()
		full := []string{"compose", "--env-file", ".env.example"}
		if _, err := os.Stat(filepath.Join(root, ".env.local")); err == nil {
			full = append(full, "--env-file", ".env.local")
		}
		cmd := exec.CommandContext(context.Background(), "docker", append(full, args...)...) //nolint:gosec // G204: fixed docker compose commands
		cmd.Dir = root
		cmd.Env = append(os.Environ(), env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, out)
		}
	}
	gateways := []string{"gateway-1", "gateway-2", "gateway-3", "gateway-4", "gateway-5"}
	t.Cleanup(func() {
		compose(nil, "up", "-d", "--no-deps", "--force-recreate", "--wait", "--wait-timeout", "120", "gateway-1")
		compose(nil, append([]string{"--profile", "replicas", "rm", "-fsv"}, gateways[1:]...)...)
	})

	// The compose gateways' own config, published: managed gateways need a
	// version to become ready.
	data, err := os.ReadFile(filepath.Join(root, "deploy", "compose", "gateway", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	_, tag := exportYAML(t, c, "")
	importYAML(t, c, 200, data, tag, "?note=TestComposeManagedGateways")
	_, tag = validate(t, c)
	if a := c.call("POST", "/config/publish", map[string]any{"note": "TestComposeManagedGateways: the compose config"}, "If-Match", tag); a.status != 200 && a.status != 409 {
		t.Fatalf("publish: %d %s", a.status, a.body)
	}
	managedEnv := []string{"SPILLWAY_GATEWAY_CONFIG=", "SPILLWAY_GATEWAY_IDENTITY=", "SPILLWAY_GATEWAY_CONTROL_PLANE=controlplane:9090"}
	compose(managedEnv, append([]string{"--profile", "replicas", "up", "-d", "--no-deps", "--force-recreate", "--wait", "--wait-timeout", "120"}, gateways...)...)

	// A new version: the settings' redis_budget flips, which changes no
	// behaviour the other tests rely on.
	a := c.must(200, "GET", "/config/settings", nil)
	var st struct{ Ratelimit map[string]any }
	a.decode(t, &st)
	if st.Ratelimit == nil {
		st.Ratelimit = map[string]any{}
	}
	st.Ratelimit["redis_budget"] = map[bool]string{true: "6ms", false: "5ms"}[st.Ratelimit["redis_budget"] == "5ms"]
	c.must(200, "PUT", "/config/settings", map[string]any{"ratelimit": st.Ratelimit}, "If-Match", a.etag())
	_, tag = validate(t, c)
	var v struct{ Version int64 }
	c.must(200, "POST", "/config/publish", map[string]any{"note": "TestComposeManagedGateways: a new version"}, "If-Match", tag).decode(t, &v)

	gw1 := &proc{name: "gateway-1", admin: "http://127.0.0.1:8001"}
	eventually(t, fmt.Sprintf("gateway-1 at version %d", v.Version), func() bool { return gw1.metric("gw_config_version") == float64(v.Version) })
	if status("http://127.0.0.1:8001/readyz") != 200 || gw1.metric("gw_identity_version") < 1 {
		t.Errorf("gateway-1: readyz %d, identity version %v", status("http://127.0.0.1:8001/readyz"), gw1.metric("gw_identity_version"))
	}
	// gateway-2 … gateway-5 publish no ports: their logs show the apply.
	applied := fmt.Sprintf(`"msg":"config applied","source":"stream","version":%d`, v.Version)
	for _, g := range gateways[1:] {
		eventually(t, g+" logged "+applied, func() bool {
			cmd := exec.CommandContext(context.Background(), "docker", "compose", "--env-file", ".env.example", "logs", "--no-log-prefix", g) //nolint:gosec // G204: fixed docker compose commands
			cmd.Dir = root
			out, err := cmd.Output()
			return err == nil && strings.Contains(string(out), applied)
		})
	}
}
