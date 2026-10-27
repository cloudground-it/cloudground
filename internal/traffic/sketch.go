package traffic

import (
	"crypto/rand"
	"encoding/hex"
	"math"
	"math/bits"
)

// HyperLogLog with 2^precision one-byte registers (SPEC §4).
const (
	precision = 12
	registers = 1 << precision
)

// Sketch counts a day's distinct visitors. Salt makes its hashes useless
// outside the day; it goes with the sketch.
type Sketch struct {
	Salt string `json:"salt"`
	Regs []byte `json:"regs"`
}

// NewSketch is an empty sketch with a fresh salt.
func NewSketch() *Sketch {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand does not fail on a working system
	return &Sketch{Salt: hex.EncodeToString(b[:]), Regs: make([]byte, registers)}
}

// Add counts one (address, user agent) pair.
func (s *Sketch) Add(addr, ua string) {
	if len(s.Regs) != registers {
		s.Regs = make([]byte, registers)
	}
	h := fnv64(s.Salt, addr, ua)
	idx := h >> (64 - precision)
	w := h<<precision | 1<<(precision-1) // never all zero
	rank := byte(bits.LeadingZeros64(w) + 1)
	if rank > s.Regs[idx] {
		s.Regs[idx] = rank
	}
}

// Estimate is the number of distinct pairs added.
func (s *Sketch) Estimate() int64 {
	if len(s.Regs) != registers {
		return 0
	}
	m := float64(registers)
	sum, zeros := 0.0, 0
	for _, r := range s.Regs {
		sum += math.Ldexp(1, -int(r))
		if r == 0 {
			zeros++
		}
	}
	alpha := 0.7213 / (1 + 1.079/m)
	e := alpha * m * m / sum
	if e <= 2.5*m && zeros > 0 {
		e = m * math.Log(m/float64(zeros))
	}
	return int64(math.Round(e))
}

// fnv64 is FNV-1a over the parts (a zero byte between them), mixed by the
// SplitMix64 finaliser so the leading bits are spread.
func fnv64(parts ...string) uint64 {
	const prime = 1099511628211
	h := uint64(14695981039346656037)
	for i, p := range parts {
		if i > 0 {
			h *= prime // a zero byte between the parts
		}
		for j := 0; j < len(p); j++ {
			h ^= uint64(p[j])
			h *= prime
		}
	}
	h ^= h >> 30
	h *= 0xbf58476d1ce4e5b9
	h ^= h >> 27
	h *= 0x94d049bb133111eb
	h ^= h >> 31
	return h
}
