package proxy

import (
	"container/list"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
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
// wraps (GW-26, ADR-0030), in all and per client address (ADR-0071). A
// connection counts from Accept to Close, upgraded ones included. At the
// cap, a new connection takes the place of the one that has waited longest
// without a request in progress, idle between requests or not yet sent its
// first, which is closed (ADR-0033). When every connection has a request in
// progress, the new one never reaches the server and is never queued: it
// gets a fixed 503 as soon as its request starts, within shedDeadline, or a
// reset when maxShedding connections are already being shed. The cap per
// address is checked first, with the same rule applied to that address's
// connections only. The address is the TCP peer's, IPv4 or the /64 of IPv6;
// peers among the trusted proxies have no cap of their own (SEC-05). The
// server's ConnState must be ConnLimit.ConnState.
type ConnLimit struct {
	max      int64
	maxPerIP int64
	trusted  TrustedProxies
	open     resilience.Gate
	shedding resilience.Gate
	rec      *telemetry.Recorder
	log      *slog.Logger

	// Counts since the last log line of each kind, logged at most once a
	// second, never with an address.
	shed, closed, shedPerIP, closedPerIP everySecond

	mu      sync.Mutex
	waiting list.List                   // of *clientConn without a request in progress, longest waiting first
	addrs   map[netip.Prefix]*addrConns // by client address, entries with open connections only
}

// addrConns are one client address's open connections.
type addrConns struct {
	key     netip.Prefix
	open    int64
	waiting list.List // of its *clientConn without a request in progress, longest waiting first
}

// everySecond counts events for a log line written at most once a second.
type everySecond struct {
	n, at atomic.Int64 // events since the last line; when it went out, in Unix nanoseconds
}

// add counts an event and returns the count to log, or 0 if a line went
// out less than a second ago.
func (e *everySecond) add() int64 {
	e.n.Add(1)
	now, last := time.Now().UnixNano(), e.at.Load()
	if now-last < int64(time.Second) || !e.at.CompareAndSwap(last, now) {
		return 0
	}
	return e.n.Swap(0)
}

// NewConnLimit returns a cap of max open connections, and of maxPerIP from
// one client address outside trusted, that records capped connections with
// rec and logs them to log at most once a second.
func NewConnLimit(max, maxPerIP int64, trusted TrustedProxies, rec *telemetry.Recorder, log *slog.Logger) *ConnLimit {
	return &ConnLimit{max: max, maxPerIP: maxPerIP, trusted: trusted, rec: rec, log: log, addrs: map[netip.Prefix]*addrConns{}}
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
		cc := &clientConn{countedConn: countedConn{Conn: c}}
		cc.release = sync.OnceFunc(func() { ln.l.release(cc) })
		if !ln.l.acquireAddr(cc) {
			ln.l.shedConn(c, ln.tls, telemetry.CapPerIP)
			continue
		}
		if !ln.l.acquire() {
			ln.l.dropAddr(cc)
			ln.l.shedConn(c, ln.tls, telemetry.CapGlobal)
			continue
		}
		return cc, nil
	}
}

// clientConn is a client connection the cap counts. Its fields are guarded
// by the ConnLimit's mu.
type clientConn struct {
	countedConn
	addr        *addrConns    // nil for a trusted proxy's
	waiting     *list.Element // in the ConnLimit's waiting list
	addrWaiting *list.Element // in its address's waiting list
	released    bool          // closed: it waits no more
}

// addrKey returns the client address c counts against, and false if c has
// none: a trusted proxy's connection, or one that is not TCP.
func (l *ConnLimit) addrKey(c net.Conn) (netip.Prefix, bool) {
	a, ok := c.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return netip.Prefix{}, false
	}
	return addrKey(a.AddrPort().Addr(), l.trusted)
}

// addrKey returns the address ip counts against: itself if IPv4, or its
// /64 if IPv6, since one client usually holds a /64 (ADR-0071). A trusted
// proxy carries many clients and has none.
func addrKey(ip netip.Addr, trusted TrustedProxies) (netip.Prefix, bool) {
	ip = ip.Unmap().WithZone("")
	if trusted.trusts(ip) {
		return netip.Prefix{}, false
	}
	if ip.Is4() {
		return netip.PrefixFrom(ip, 32), true
	}
	p, err := ip.Prefix(64)
	return p, err == nil
}

// acquireAddr takes a place for c among its address's connections, closing
// that address's waiting connections for it while its cap is reached.
func (l *ConnLimit) acquireAddr(c *clientConn) bool {
	key, ok := l.addrKey(c.Conn)
	if !ok {
		return true
	}
	for {
		l.mu.Lock()
		a := l.addrs[key]
		if a == nil {
			a = &addrConns{key: key}
			l.addrs[key] = a
		}
		if a.open < l.maxPerIP {
			a.open++
			c.addr = a
			l.mu.Unlock()
			return true
		}
		e := a.waiting.Front()
		if e == nil {
			l.mu.Unlock()
			return false
		}
		w := e.Value.(*clientConn)
		l.unwait(w)
		l.mu.Unlock()
		_ = w.Close() // frees its place
		l.noteClosed(telemetry.CapPerIP)
	}
}

// acquire takes a place for a new connection, closing waiting connections
// for it while the cap is reached.
func (l *ConnLimit) acquire() bool {
	for !l.open.TryAcquire(l.max) {
		l.mu.Lock()
		e := l.waiting.Front()
		if e == nil {
			l.mu.Unlock()
			return false
		}
		c := e.Value.(*clientConn)
		l.unwait(c)
		l.mu.Unlock()
		_ = c.Close() // frees its place
		l.noteClosed(telemetry.CapGlobal)
	}
	return true
}

// release frees c's places once it is closed.
func (l *ConnLimit) release(c *clientConn) {
	l.mu.Lock()
	c.released = true
	l.unwait(c)
	l.mu.Unlock()
	l.dropAddr(c)
	l.open.Release()
}

// dropAddr frees c's place among its address's connections.
func (l *ConnLimit) dropAddr(c *clientConn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if a := c.addr; a != nil {
		c.addr = nil
		if a.open--; a.open == 0 {
			delete(l.addrs, a.key)
		}
	}
}

// unwait takes c off the waiting lists; l.mu must be held.
func (l *ConnLimit) unwait(c *clientConn) {
	if c.waiting != nil {
		l.waiting.Remove(c.waiting)
		c.waiting = nil
	}
	if c.addrWaiting != nil {
		c.addr.waiting.Remove(c.addrWaiting)
		c.addrWaiting = nil
	}
}

// ConnState is the traffic server's http.Server.ConnState: it keeps the
// lists of connections without a request in progress, new or idle, in the
// order they started waiting (ADR-0033).
func (l *ConnLimit) ConnState(nc net.Conn, state http.ConnState) {
	if tc, ok := nc.(*tls.Conn); ok {
		nc = tc.NetConn()
	}
	c, ok := nc.(*clientConn)
	if !ok {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if c.released {
		return
	}
	l.unwait(c)
	if state == http.StateNew || state == http.StateIdle {
		c.waiting = l.waiting.PushBack(c)
		if c.addr != nil {
			c.addrWaiting = c.addr.waiting.PushBack(c)
		}
	}
}

// noteClosed records a waiting connection closed at the cap named which
// and logs the count at most once a second.
func (l *ConnLimit) noteClosed(which string) {
	l.rec.ConnectionCapped(which, telemetry.ActionClosedWaiting)
	if which == telemetry.CapPerIP {
		if n := l.closedPerIP.add(); n > 0 {
			l.log.Info("closing waiting connections at the per-address connection cap", "connections", n, "max_connections_per_ip", l.maxPerIP)
		}
		return
	}
	if n := l.closed.add(); n > 0 {
		l.log.Info("closing waiting connections at the connection cap", "connections", n, "max_connections", l.max)
	}
}

// shedConn answers c, accepted past the cap named which, on its own
// goroutine, or resets it when the shed path is full.
func (l *ConnLimit) shedConn(c net.Conn, tlsConfig *tls.Config, which string) {
	l.rec.ShedConnection()
	l.rec.ConnectionCapped(which, telemetry.ActionShed)
	l.noteShed(which)
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

// noteShed logs the count of connections shed at the cap named which at
// most once a second.
func (l *ConnLimit) noteShed(which string) {
	if which == telemetry.CapPerIP {
		if n := l.shedPerIP.add(); n > 0 {
			l.log.Warn("shedding connections at the per-address connection cap", "connections", n, "max_connections_per_ip", l.maxPerIP)
		}
		return
	}
	if n := l.shed.add(); n > 0 {
		l.log.Warn("shedding connections", "connections", n, "max_connections", l.max)
	}
}

// answerShed writes the fixed 503 on c, after a TLS handshake when tlsConfig
// is set, then half-closes it and reads until the client closes, so that
// unread request bytes don't turn the close into a reset that could discard
// the answer. Everything ends within shedDeadline.
//
// The answer waits for the request's first byte, and the rest is never
// parsed: an HTTP/1.1 client may fail a request whose answer arrived before
// it sent it, as unsolicited (Go's transport did for 0.3% to 1% of shed
// connections inside the compose network, ADR-0032). A client that sends
// nothing within the deadline gets no answer.
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
