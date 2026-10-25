package anonymize

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"sync"
)

// keyer derives deterministic digests from a key. It is the only place that
// touches HMAC. A keyer is immutable after construction and safe for
// concurrent use; the per-goroutine HMAC state lives in a sync.Pool.
type keyer struct {
	pool sync.Pool
}

type macState struct {
	h   hash.Hash
	buf [512]byte
	sum [sha256.Size]byte
}

// newKeyer returns a keyer for key. The key is copied.
func newKeyer(key []byte) *keyer {
	kc := append([]byte(nil), key...)
	k := &keyer{}
	k.pool.New = func() any { return &macState{h: hmac.New(sha256.New, kc)} }
	return k
}

// deriveKey returns the HMAC key for secret and salt. An empty salt yields the
// secret itself, so an unsalted engine is keyed by the plain secret. A salt is
// mixed in through HMAC rather than concatenation, so ("ab","c") and
// ("a","bc") can never collide.
func deriveKey(secret []byte, salt string) []byte {
	if salt == "" {
		return secret
	}
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("mirage/anonymize/salt\x00"))
	m.Write([]byte(salt))
	return m.Sum(nil)
}

// digest returns HMAC-SHA256(key, domain || 0x00 || value). The domain
// separates the output spaces of different transformations ("hash" and
// "fake/email" never share a digest for the same value).
func (k *keyer) digest(domain, value string) [sha256.Size]byte {
	m := k.pool.Get().(*macState)
	m.h.Reset()
	n := copy(m.buf[:], domain)
	m.buf[n] = 0
	n++
	for {
		c := copy(m.buf[n:], value)
		m.h.Write(m.buf[:n+c])
		value = value[c:]
		n = 0
		if len(value) == 0 {
			break
		}
	}
	m.h.Sum(m.sum[:0])
	out := m.sum
	k.pool.Put(m)
	return out
}

// hashToken returns the deterministic token for v: "h_" followed by 16 hex
// characters. The token is URL-safe, so it can replace path segments, query
// values and cookie values without escaping.
func (k *keyer) hashToken(v string) string {
	d := k.digest("hash", v)
	var b [18]byte
	b[0], b[1] = 'h', '_'
	for i := 0; i < 8; i++ {
		b[2+2*i] = hexDigits[d[i]>>4]
		b[3+2*i] = hexDigits[d[i]&0xf]
	}
	return string(b[:])
}

// prng is a deterministic pseudo-random byte stream seeded by a digest. It is
// not a CSPRNG and is only used to pick fake data; the unpredictability comes
// from the HMAC seed. When the 32 seed bytes are used up it rehashes itself,
// so arbitrarily long fake values are possible.
type prng struct {
	state [sha256.Size]byte
	pos   int
	ctr   uint64
}

func newPRNG(seed [sha256.Size]byte) prng { return prng{state: seed} }

func (p *prng) byte() byte {
	if p.pos == len(p.state) {
		var buf [sha256.Size + 8]byte
		copy(buf[:], p.state[:])
		binary.BigEndian.PutUint64(buf[sha256.Size:], p.ctr)
		p.ctr++
		p.state = sha256.Sum256(buf[:])
		p.pos = 0
	}
	b := p.state[p.pos]
	p.pos++
	return b
}

// intn returns a value in [0, n) for 0 < n <= 1<<24 with negligible bias.
func (p *prng) intn(n int) int {
	v := uint32(p.byte())<<24 | uint32(p.byte())<<16 | uint32(p.byte())<<8 | uint32(p.byte())
	return int(uint64(v) * uint64(n) >> 32)
}

// digit returns an unbiased decimal digit as a byte value 0-9.
func (p *prng) digit() byte {
	for {
		if b := p.byte(); b < 250 {
			return b % 10
		}
	}
}

// letter returns an unbiased value in 0-25.
func (p *prng) letter() byte {
	for {
		if b := p.byte(); b < 234 {
			return b % 26
		}
	}
}
