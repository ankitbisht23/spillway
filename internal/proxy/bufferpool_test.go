package proxy

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

// TestCopyBuffersNotCarried: a response body copied through a reused buffer
// carries only its own bytes, never a previous response's: after a buffer
// was filled with other bytes, and with requests of different lengths at
// once, short bodies, bodies sent in small flushed pieces and bodies longer
// than a buffer included (NFR-01, ADR-0073 §D7).
func TestCopyBuffersNotCarried(t *testing.T) {
	// Each body is the request's byte b repeated n times, written in
	// pieces of at most piece bytes, each flushed.
	u := &upstream{name: "bodies"}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		n, _ := strconv.Atoi(q.Get("n"))
		piece, _ := strconv.Atoi(q.Get("piece"))
		body := bytes.Repeat([]byte(q.Get("b")), n)
		for len(body) > 0 {
			k := min(piece, len(body))
			_, _ = w.Write(body[:k])
			w.(http.Flusher).Flush()
			body = body[k:]
		}
	}))
	t.Cleanup(u.Close)
	g := newGateway(t, newResolver(u), oneUpstream(u))

	get := func(t *testing.T, b byte, n, piece int) {
		t.Helper()
		got := g.send(t, http.MethodGet, fmt.Sprintf("/?b=%c&n=%d&piece=%d", b, n, piece), nil)
		if want := string(bytes.Repeat([]byte{b}, n)); got.status != http.StatusOK || got.body != want {
			t.Errorf("b=%c n=%d piece=%d: status %d, %d bytes, want 200 and %d bytes of %c only", b, n, piece, got.status, len(got.body), n, b)
		}
	}

	// Buffers full of another response's bytes go back to the pool, then
	// a full buffer's worth and more of 'A', then short bodies.
	for range 4 {
		b := copyBuffers.Get()
		for i := range b {
			b[i] = 'Z'
		}
		copyBuffers.Put(b)
	}
	get(t, 'A', 3*copyBufferSize+7, copyBufferSize)
	get(t, 'b', 2, 1)
	get(t, 'c', 1, 1)
	get(t, 'd', 100, 7)

	// Requests at once, each with its own byte and length.
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			for j := range 20 {
				get(t, byte('a'+i), (i*7919+j*104729)%(2*copyBufferSize)+1, 64*(1+(i*31+j*17)%64))
			}
		})
	}
	wg.Wait()
}

// TestBufferPoolSize: the pool hands out copyBufferSize buffers and keeps
// only buffers of that capacity.
func TestBufferPoolSize(t *testing.T) {
	if b := copyBuffers.Get(); len(b) != copyBufferSize {
		t.Fatalf("got a %d-byte buffer, want %d", len(b), copyBufferSize)
	}
	p := &bufferPool{}
	p.Put(make([]byte, 10))
	if got := p.pool.Get(); got != nil {
		t.Errorf("a 10-byte buffer was kept")
	}
	p.Put(make([]byte, 5, copyBufferSize))
	if got, ok := p.pool.Get().(*[]byte); ok && len(*got) != copyBufferSize {
		t.Errorf("a buffer came back with length %d, want %d", len(*got), copyBufferSize)
	}
}
