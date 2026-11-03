package proxy

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/config"
)

func TestServerLimits(t *testing.T) {
	srv := NewServer(http.NotFoundHandler(), slog.New(slog.DiscardHandler))
	if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 || srv.WriteTimeout <= 0 || srv.IdleTimeout <= 0 || srv.MaxHeaderBytes <= 0 {
		t.Fatalf("a GW-28 limit is unset: %+v", srv)
	}
	// Route limits (GW-03, GW-11) are validated against these.
	if config.MaxHeaderBytes != srv.MaxHeaderBytes || config.MaxTotalTimeout >= srv.WriteTimeout {
		t.Errorf("config allows max_header_bytes up to %d and total up to %v; the server has %d and a %v write timeout",
			config.MaxHeaderBytes, config.MaxTotalTimeout, srv.MaxHeaderBytes, srv.WriteTimeout)
	}

	srv.ReadHeaderTimeout = 200 * time.Millisecond // keep the slow-client test fast
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	// A client that never finishes its headers is cut off.
	conn := dial(t, ln.Addr().String())
	_, _ = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: x\r\n")
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	_, _ = io.ReadAll(conn)
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("slow client held the connection for %v", took)
	}

	// Oversized request headers get 431.
	conn2 := dial(t, ln.Addr().String())
	_, _ = io.WriteString(conn2, "GET / HTTP/1.1\r\nHost: x\r\nX-Big: "+strings.Repeat("a", 2*maxHeaderBytes)+"\r\n\r\n")
	line, _ := bufio.NewReader(conn2).ReadString('\n')
	if !strings.Contains(line, "431") {
		t.Errorf("oversized headers answered %q, want 431", line)
	}
}
