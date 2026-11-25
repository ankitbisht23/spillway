package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeKeycloak is an https server with a discovery document and a JWKS;
// issuer and jwksURI default to the server's own URLs when empty, and keys
// is the number of keys the JWKS serves. It returns the server and the
// path of a file holding its CA certificate.
func fakeKeycloak(t *testing.T, issuer, jwksURI string, keys int) (*httptest.Server, string) {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/realms/spillway/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		iss, jw := issuer, jwksURI
		if iss == "" {
			iss = srv.URL + "/auth/realms/spillway"
		}
		if jw == "" {
			jw = srv.URL + "/auth/realms/spillway/protocol/openid-connect/certs"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": iss, "jwks_uri": jw})
	})
	mux.HandleFunc("GET /auth/realms/spillway/protocol/openid-connect/certs", func(w http.ResponseWriter, _ *http.Request) {
		set := map[string][]map[string]string{"keys": {}}
		for range keys {
			set["keys"] = append(set["keys"], map[string]string{"kty": "RSA", "kid": "k"})
		}
		_ = json.NewEncoder(w).Encode(set)
	})
	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(ca, block, 0o600); err != nil {
		t.Fatal(err)
	}
	return srv, ca
}

func TestKindsmoke(t *testing.T) {
	cases := []struct {
		name            string
		issuer, jwksURI string // served; empty = the server's own
		keys            int
		want            int
		wantOut         []string
	}{
		{name: "pass", keys: 2, want: 0, wantOut: []string{"PASS  keycloak discovery", "PASS  keycloak JWKS", "2 keys"}},
		{name: "other issuer", issuer: "https://keycloak.example/auth/realms/spillway", keys: 1, want: 1,
			wantOut: []string{"FAIL  keycloak discovery", `issuer "https://keycloak.example`}},
		{name: "http jwks", jwksURI: "http://keycloak:8080/certs", keys: 1, want: 1,
			wantOut: []string{"FAIL  keycloak discovery", "not an https URL"}},
		{name: "no keys", keys: 0, want: 1, wantOut: []string{"PASS  keycloak discovery", "FAIL  keycloak JWKS", "no keys"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, ca := fakeKeycloak(t, tc.issuer, tc.jwksURI, tc.keys)
			var out, errOut bytes.Buffer
			code := runKindsmoke(t.Context(), []string{"-keycloak-issuer", srv.URL + "/auth/realms/spillway", "-ca", ca}, &out, &errOut)
			if code != tc.want {
				t.Fatalf("exit %d, want %d; stdout:\n%s\nstderr:\n%s", code, tc.want, out.String(), errOut.String())
			}
			for _, w := range tc.wantOut {
				if !strings.Contains(out.String(), w) {
					t.Errorf("stdout lacks %q:\n%s", w, out.String())
				}
			}
		})
	}
}

// TestKindsmokeTrustsOnlyTheCA: a server whose certificate the -ca file did
// not sign fails the discovery check.
func TestKindsmokeTrustsOnlyTheCA(t *testing.T) {
	srv, _ := fakeKeycloak(t, "", "", 1)
	// httptest servers share one certificate, so the other CA is made here.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "other CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	otherCA := filepath.Join(t.TempDir(), "other.pem")
	if err := os.WriteFile(otherCA, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := runKindsmoke(t.Context(), []string{"-keycloak-issuer", srv.URL + "/auth/realms/spillway", "-ca", otherCA}, &out, &errOut); code != 1 {
		t.Fatalf("exit %d, want 1; stdout:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "FAIL  keycloak discovery") || !strings.Contains(out.String(), "certificate") {
		t.Errorf("stdout:\n%s", out.String())
	}
}

func TestKindsmokeFlags(t *testing.T) {
	_, ca := fakeKeycloak(t, "", "", 1)
	for _, args := range [][]string{
		{"-ca", ca},
		{"-keycloak-issuer", "http://keycloak:8080/auth/realms/spillway", "-ca", ca},
		{"-keycloak-issuer", "https://keycloak/auth/realms/spillway"},
		{"-keycloak-issuer", "https://keycloak/auth/realms/spillway", "-ca", filepath.Join(t.TempDir(), "missing.pem")},
		{"-keycloak-issuer", "https://keycloak/auth/realms/spillway", "-ca", ca, "extra"},
	} {
		var out, errOut bytes.Buffer
		if code := runKindsmoke(t.Context(), args, &out, &errOut); code != 2 {
			t.Errorf("%q: exit %d, want 2", args, code)
		}
	}
}

// TestKindsmokeChart: the chart's checks (ADR-0086): the gateway URL must
// answer 200, every -closed address must not accept a connection, and
// Prometheus's active targets of each job must all be up and as many as
// -targets says, within -targets-wait.
func TestKindsmokeChart(t *testing.T) {
	srv, ca := fakeKeycloak(t, "", "", 1)
	// httptest servers share one certificate, so ca trusts this one too.
	gw := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bytes/16" {
			http.Error(w, "no route", http.StatusNotFound)
			return
		}
		_, _ = w.Write(make([]byte, 16))
	}))
	t.Cleanup(gw.Close)
	var up atomic.Int32 // gateway targets up; the last one comes up late
	up.Store(2)
	promSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/targets" || r.URL.Query().Get("state") != "active" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var ts []map[string]any
		for i := range 3 {
			h := "down"
			if int32(i) < up.Load() {
				h = "up"
			}
			ts = append(ts, map[string]any{"labels": map[string]string{"job": "gateway"}, "health": h})
		}
		ts = append(ts, map[string]any{"labels": map[string]string{"job": "controlplane"}, "health": "up"},
			map[string]any{"labels": map[string]string{"job": "redis"}, "health": "down"})
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"activeTargets": ts}})
	}))
	t.Cleanup(promSrv.Close)
	// An address that accepts connections, and one that does not.
	lc := &net.ListenConfig{}
	open, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = open.Close() })
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := l.Addr().String()
	_ = l.Close()

	base := []string{"-keycloak-issuer", srv.URL + "/auth/realms/spillway", "-ca", ca}
	run := func(extra ...string) (int, string) {
		var out, errOut bytes.Buffer
		code := runKindsmoke(t.Context(), append(slices.Clone(base), extra...), &out, &errOut)
		return code, out.String() + errOut.String()
	}

	go func() { time.Sleep(1500 * time.Millisecond); up.Store(3) }()
	code, out := run("-gateway", gw.URL+"/bytes/16", "-closed", refused, "-prometheus", promSrv.URL, "-targets", "gateway=3,controlplane=1", "-targets-wait", "20s")
	if code != 0 || !strings.Contains(out, "PASS  gateway Service over https -> 200") || !strings.Contains(out, "16 bytes") ||
		!strings.Contains(out, "PASS  admin "+refused) || !strings.Contains(out, "controlplane 1 of 1 up (want 1), gateway 3 of 3 up (want 3)") {
		t.Fatalf("pass: exit %d:\n%s", code, out)
	}

	code, out = run("-gateway", gw.URL+"/missing", "-closed", open.Addr().String()+","+refused, "-prometheus", promSrv.URL, "-targets", "gateway=4", "-targets-wait", "1s")
	if code != 1 || !strings.Contains(out, "FAIL  gateway Service over https -> 200") || !strings.Contains(out, "404") ||
		!strings.Contains(out, "FAIL  admin "+open.Addr().String()) || !strings.Contains(out, "connected to") ||
		!strings.Contains(out, "PASS  admin "+refused) || !strings.Contains(out, "FAIL  prometheus targets up") || !strings.Contains(out, "gateway 3 of 3 up (want 4)") {
		t.Errorf("fail: exit %d:\n%s", code, out)
	}

	for _, extra := range [][]string{
		{"-gateway", "http://spillway-gateway/bytes/16"},
		{"-prometheus", promSrv.URL},
		{"-targets", "gateway=3"},
		{"-prometheus", promSrv.URL, "-targets", "gateway"},
		{"-prometheus", promSrv.URL, "-targets", "gateway=0"},
	} {
		if code, out := run(extra...); code != 2 {
			t.Errorf("%q: exit %d, want 2:\n%s", extra, code, out)
		}
	}
}
