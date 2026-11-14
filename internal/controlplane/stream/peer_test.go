package stream

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ankitbisht23/spillway/internal/gen/configpb"
	"github.com/ankitbisht23/spillway/internal/testcert"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestStreamMTLS checks the config stream's peers end to end over gRPC
// (SEC-02, D7, ADR-0040): no client certificate, a certificate of another
// CA, an expired one, or one whose name is not one DNS label is refused
// and logged; the right certificate is accepted and both calls take the
// replica name from it; a request naming another replica is refused and
// logged with both names; no key material reaches a log line.
func TestStreamMTLS(t *testing.T) {
	var logs syncBuffer
	rec := &fakeRecorder{live: 1}
	c, _ := configServer(t, newFakeSource(1, 1), rec, slog.New(slog.NewJSONHandler(&logs, nil)))
	ctx := t.Context()

	issue := func(ca *testcert.CA, l testcert.Leaf) configpb.ConfigServiceClient {
		t.Helper()
		dir := t.TempDir()
		ca.Issue(t, dir, l)
		// Trust the stream CA, whatever signed the certificate.
		if err := os.WriteFile(filepath.Join(dir, "ca.pem"), c.ca.PEM(), 0o600); err != nil {
			t.Fatal(err)
		}
		return c.dial(loadFiles(t, dir).ClientConfig("controlplane"))
	}
	other := testcert.NewCA(t, "other CA")
	noCertCfg := loadFiles(t, func() string {
		dir := t.TempDir()
		c.ca.Issue(t, dir, testcert.Leaf{DNSNames: []string{"gateway-1"}, Client: true})
		return dir
	}()).ClientConfig("controlplane")
	noCertCfg.GetClientCertificate = nil

	for _, tc := range []struct {
		name   string
		client configpb.ConfigServiceClient
		code   codes.Code
		logged string
	}{
		{"no client certificate", c.dial(noCertCfg), codes.Unavailable, "client didn't provide a certificate"},
		{"another CA", issue(other, testcert.Leaf{DNSNames: []string{"gateway-1"}, Client: true}), codes.Unavailable, "signed by unknown authority"},
		{"expired", issue(c.ca, testcert.Leaf{DNSNames: []string{"gateway-1"}, Client: true, NotAfter: time.Now().Add(-time.Minute)}), codes.Unavailable, "certificate has expired"},
		{"no client-auth usage", issue(c.ca, testcert.Leaf{DNSNames: []string{"gateway-1"}}), codes.Unavailable, "incompatible key usage"},
		{"two names", issue(c.ca, testcert.Leaf{DNSNames: []string{"gateway-1", "gateway-2"}, Client: true}), codes.PermissionDenied, "2 DNS names"},
		{"not a DNS label", issue(c.ca, testcert.Leaf{DNSNames: []string{"gateway-1.example"}, Client: true}), codes.PermissionDenied, "is not a DNS label"},
		{"no DNS name", issue(c.ca, testcert.Leaf{Client: true}), codes.PermissionDenied, "0 DNS names"},
	} {
		before := logs.String()
		_, hbErr := tc.client.Heartbeat(ctx, &configpb.HeartbeatRequest{ReplicaId: "gateway-1"})
		s, err := tc.client.Watch(ctx, watchReq("gateway-1", 0, 0))
		if err == nil {
			_, err = recv(t, s)
		}
		if status.Code(hbErr) != tc.code || status.Code(err) != tc.code {
			t.Errorf("%s: Heartbeat %v, Watch %v; want %v", tc.name, hbErr, err, tc.code)
		}
		// The server's log line names the reason; gRPC on the client may
		// have it too, but the server's own line is what an operator reads.
		if added := strings.TrimPrefix(logs.String(), before); !strings.Contains(added, tc.logged) {
			t.Errorf("%s: the server logged %q, want a line with %q", tc.name, added, tc.logged)
		}
	}
	if len(rec.names) != 0 {
		t.Fatalf("refused peers recorded heartbeats: %v", rec.names)
	}

	// The right certificate: accepted, the name its own.
	gw := c.as("gateway-3")
	if _, err := gw.Heartbeat(ctx, &configpb.HeartbeatRequest{ReplicaId: "gateway-3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := gw.Heartbeat(ctx, &configpb.HeartbeatRequest{}); err != nil {
		t.Fatal(err)
	}
	s, err := gw.Watch(ctx, watchReq("gateway-3", 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := recv(t, s); got != "config 1" {
		t.Fatalf("Watch: %q, %v", got, err)
	}
	if len(rec.names) != 2 || rec.names[0] != "gateway-3" || rec.names[1] != "gateway-3" {
		t.Errorf("recorded as %v, want gateway-3 twice", rec.names)
	}
	// Another replica's name in the request.
	before := logs.String()
	if _, err := gw.Heartbeat(ctx, &configpb.HeartbeatRequest{ReplicaId: "gateway-1"}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("Heartbeat as gateway-1: %v", err)
	}
	if added := strings.TrimPrefix(logs.String(), before); !strings.Contains(added, `"replica":"gateway-3","replica_id":"gateway-1"`) {
		t.Errorf("the mismatch logged as %q", added)
	}
	if out := logs.String(); strings.Contains(out, "PRIVATE KEY") || strings.Contains(out, "BEGIN") {
		t.Errorf("PEM data in the logs:\n%s", out)
	}
}
