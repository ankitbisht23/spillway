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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
