package proxy

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ankitbisht23/spillway/internal/resilience"
	"github.com/ankitbisht23/spillway/internal/telemetry"
)

// The shed path for connections past the connection cap (GW-26, ADR-0030).
const (
	maxShedding  = 256         // connections in the shed path at once; past it, a reset
	shedDeadline = time.Second // for everything a shed connection gets, from accept to close
)

// overloaded is the body of every shed answer (D5).
const overloaded = `{"error":"overloaded"}`

// shedResponse is written on a shed connection once its request starts,
// without reading the rest.
var shedResponse = "HTTP/1.1 503 Service Unavailable\r\n" +
	"Retry-After: 1\r\n" +
	"Connection: close\r\n" +
	"Content-Type: application/json\r\n" +
	"X-Content-Type-Options: nosniff\r\n" +
	"Content-Length: " + strconv.Itoa(len(overloaded)) + "\r\n" +
	"\r\n" + overloaded

// ConnLimit caps the open client connections across the traffic listeners it
// wraps (GW-26, ADR-0030). A connection counts from Accept to Close, upgraded
// ones included. One accepted past the cap never reaches the server and is
// never queued: it gets a fixed 503 as soon as its request starts, within
// shedDeadline, or a reset when maxShedding connections are already being
// shed.
type ConnLimit struct {
	max      int64
	open     resilience.Gate
	shedding resilience.Gate
	rec      *telemetry.Recorder
	log      *slog.Logger
	shed     atomic.Int64 // connections shed since the last log line
	logged   atomic.Int64 // when the last log line went out, in Unix nanoseconds
}

// NewConnLimit returns a cap of max open connections that records each shed
// connection with rec and logs shedding to log at most once a second.
func NewConnLimit(max int64, rec *telemetry.Recorder, log *slog.Logger) *ConnLimit {
	return &ConnLimit{max: max, rec: rec, log: log}
}

// Open returns the number of open connections.
func (l *ConnLimit) Open() int64 {
	return l.open.InUse()
}

// Listener wraps ln, whose connections then count against the cap. For a TLS
// listener, ln is its TCP side (http.Server.ServeTLS adds TLS) and tlsConfig
// is the server's: a shed connection first gets a handshake with a copy of it
// whose ALPN offers only HTTP/1.1, so that the 503 can be written as HTTP/1.1.
func (l *ConnLimit) Listener(ln net.Listener, tlsConfig *tls.Config) net.Listener {
	if tlsConfig != nil {
		tlsConfig = tlsConfig.Clone()
		tlsConfig.NextProtos = []string{"http/1.1"}
	}
	return &limitListener{Listener: ln, l: l, tls: tlsConfig}
}

type limitListener struct {
	net.Listener
	l   *ConnLimit
	tls *tls.Config // nil on a plain-HTTP listener
}

func (ln *limitListener) Accept() (net.Conn, error) {
	for {
		c, err := ln.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if ln.l.open.TryAcquire(ln.l.max) {
			return &countedConn{Conn: c, release: sync.OnceFunc(ln.l.open.Release)}, nil
		}
		ln.l.shedConn(c, ln.tls)
	}
}

// shedConn answers c, accepted past the cap, on its own goroutine, or resets
// it when the shed path is full.
func (l *ConnLimit) shedConn(c net.Conn, tlsConfig *tls.Config) {
	l.rec.ShedConnection()
	l.note()
	if !l.shedding.TryAcquire(maxShedding) {
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
		_ = c.Close()
		return
	}
	go func() {
		defer l.shedding.Release()
		answerShed(c, tlsConfig)
	}()
}

// note counts a shed connection and logs the count at most once a second.
func (l *ConnLimit) note() {
	l.shed.Add(1)
	now, last := time.Now().UnixNano(), l.logged.Load()
	if now-last < int64(time.Second) || !l.logged.CompareAndSwap(last, now) {
		return
	}
	l.log.Warn("shedding connections", "connections", l.shed.Swap(0), "max_connections", l.max)
}

// answerShed writes the fixed 503 on c, after a TLS handshake when tlsConfig
// is set, then half-closes it and reads until the client closes, so that
// unread request bytes don't turn the close into a reset that could discard
// the answer. Everything ends within shedDeadline.
//
// The answer waits for the request's first byte, and the rest is never
// parsed: an HTTP/1.1 client may fail a request whose answer arrived before
// it sent it, as unsolicited (Go's transport did for 0.3% to 1% of shed
// connections inside the compose network). A client that sends nothing within the
// deadline gets no answer.
func answerShed(c net.Conn, tlsConfig *tls.Config) {
	defer func() { _ = c.Close() }()
	deadline := time.Now().Add(shedDeadline)
	_ = c.SetDeadline(deadline)
	var rw io.ReadWriter = c
	if tlsConfig != nil {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		tc := tls.Server(c, tlsConfig)
		if tc.HandshakeContext(ctx) != nil {
			return
		}
		rw = tc
	}
	if _, err := rw.Read(make([]byte, 1)); err != nil {
		return
	}
	if _, err := io.WriteString(rw, shedResponse); err != nil {
		return
	}
	if tc, ok := rw.(*tls.Conn); ok {
		_ = tc.CloseWrite() // close_notify
	}
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
	_, _ = io.Copy(io.Discard, c)
}
