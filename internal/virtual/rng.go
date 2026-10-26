package virtual

import (
	"math/bits"
	"math/rand/v2"
	"sync/atomic"
)

// rng is a goroutine-safe random source that never takes a lock. Unseeded it
// delegates to math/rand/v2's per-thread generator. Seeded it draws from a
// counter-based splitmix64 stream, so a single goroutine sees a reproducible
// sequence (concurrent callers interleave it).
type rng struct {
	seeded bool
	seed   uint64
	n      atomic.Uint64
}

// init sets the seed; 0 selects the unseeded source.
func (r *rng) init(seed uint64) {
	r.seeded, r.seed = seed != 0, seed
}

func (r *rng) uint64() uint64 {
	if !r.seeded {
		return rand.Uint64()
	}
	z := r.seed + r.n.Add(1)*0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// float64 returns a uniform value in [0, 1).
func (r *rng) float64() float64 { return float64(r.uint64()>>11) / (1 << 53) }

// intn returns a uniform value in [0, n); n must be positive.
func (r *rng) intn(n int) int {
	hi, _ := bits.Mul64(r.uint64(), uint64(n))
	return int(hi)
}
