package store

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
)

const (
	// compressMinSize is the largest body stored as-is without trying to
	// compress it: gzip's header and a handful of bytes make smaller bodies
	// grow.
	compressMinSize = 1024
	// sniffMinSize and sniffWindow implement a cheap early-out for big bodies
	// that are already compressed (images, archives): compress three small
	// windows (head, middle, tail) first and give up when they barely shrink.
	sniffMinSize = 16 << 10
	sniffWindow  = 2 << 10
	// maxInflateHint caps the buffer pre-sized from the gzip trailer.
	maxInflateHint = 256 << 20

	// Per-row flag bits in exchanges.body_flags.
	flagReqGzip  = 1 << 0
	flagRespGzip = 1 << 1
)

var (
	gzipWriters = sync.Pool{New: func() any {
		w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
		return w
	}}
	gzipReaders sync.Pool
)

// packBody returns the bytes to store for a body and whether they are
// gzip-compressed. Compression is used only when it makes the body smaller, so
// callers can store the result unconditionally.
func packBody(b []byte) (out []byte, compressed bool) {
	if len(b) <= compressMinSize {
		return b, false
	}
	if len(b) >= sniffMinSize && !looksCompressible(b) {
		return b, false
	}
	c := gzipBytes(b)
	if len(c) >= len(b) {
		return b, false
	}
	return c, true
}

// looksCompressible compresses a sample of three windows of b and reports
// whether it shrank by at least 10%. Sampling several places, not just the
// start, keeps bodies with an incompressible header but a compressible rest
// from being skipped.
func looksCompressible(b []byte) bool {
	sample := make([]byte, 0, 3*sniffWindow)
	sample = append(sample, b[:sniffWindow]...)
	mid := (len(b) - sniffWindow) / 2
	sample = append(sample, b[mid:mid+sniffWindow]...)
	sample = append(sample, b[len(b)-sniffWindow:]...)
	return len(gzipBytes(sample)) <= len(sample)*9/10
}

// gzipBytes compresses src with pooled writers at the fastest level.
func gzipBytes(src []byte) []byte {
	buf := bytes.NewBuffer(make([]byte, 0, len(src)/3+64))
	zw := gzipWriters.Get().(*gzip.Writer)
	zw.Reset(buf)
	// Writes to a bytes.Buffer cannot fail.
	_, _ = zw.Write(src)
	_ = zw.Close()
	gzipWriters.Put(zw)
	return buf.Bytes()
}

// unpackBody reverses packBody.
func unpackBody(b []byte, compressed bool) ([]byte, error) {
	if !compressed || len(b) == 0 {
		return b, nil
	}
	src := bytes.NewReader(b)
	zr, _ := gzipReaders.Get().(*gzip.Reader)
	var err error
	if zr == nil {
		zr, err = gzip.NewReader(src)
	} else {
		err = zr.Reset(src)
	}
	if err != nil {
		return nil, fmt.Errorf("inflate body: %w", err)
	}
	defer gzipReaders.Put(zr)

	hint := 0
	if len(b) >= 4 {
		if n := binary.LittleEndian.Uint32(b[len(b)-4:]); n <= maxInflateHint {
			hint = int(n)
		}
	}
	out := bytes.NewBuffer(make([]byte, 0, hint))
	if _, err := out.ReadFrom(zr); err != nil {
		return nil, fmt.Errorf("inflate body: %w", err)
	}
	return out.Bytes(), nil
}
