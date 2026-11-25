package proxy

import "sync"

// copyBufferSize is the size of httputil.ReverseProxy's own body copy
// buffer, which it allocates for every response without a BufferPool.
const copyBufferSize = 32 * 1024

// copyBuffers reuses the reverse proxy's body copy buffers across requests
// (NFR-01, ADR-0073 §D7). The proxy reads into a buffer and writes only the
// bytes each read returned, before it gives the buffer back, so a reused
// buffer's old bytes are never sent.
var copyBuffers = &bufferPool{pool: sync.Pool{New: func() any {
	b := make([]byte, copyBufferSize)
	return &b
}}}

// bufferPool is an httputil.BufferPool of copyBufferSize buffers.
type bufferPool struct{ pool sync.Pool }

func (p *bufferPool) Get() []byte { return *p.pool.Get().(*[]byte) }

func (p *bufferPool) Put(b []byte) {
	if cap(b) != copyBufferSize {
		return
	}
	b = b[:copyBufferSize]
	p.pool.Put(&b)
}
