package blocklist

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"math/bits"
	"net/netip"
	"slices"
	"strings"
)

// Countries (issue #25, SPEC §6): the data is DB-IP's "IP to Country Lite"
// (CC BY 4.0), a CSV of start,end,country rows for IPv4 and IPv6.

// ValidCountry reports whether c is an ISO 3166-1 alpha-2 code as DB-IP
// writes them: two upper-case letters.
func ValidCountry(c string) bool {
	return len(c) == 2 && c[0] >= 'A' && c[0] <= 'Z' && c[1] >= 'A' && c[1] <= 'Z'
}

// u128 is an address as a 128-bit number (IPv4 in the low 32 bits).
type u128 struct{ hi, lo uint64 }

func toU128(a netip.Addr) u128 {
	if a.Is4() {
		b := a.As4()
		return u128{lo: uint64(binary.BigEndian.Uint32(b[:]))}
	}
	b := a.As16()
	return u128{hi: binary.BigEndian.Uint64(b[:8]), lo: binary.BigEndian.Uint64(b[8:])}
}

func fromU128(v u128, v4 bool) netip.Addr {
	if v4 {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(v.lo))
		return netip.AddrFrom4(b)
	}
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], v.hi)
	binary.BigEndian.PutUint64(b[8:], v.lo)
	return netip.AddrFrom16(b)
}

func (v u128) cmp(w u128) int {
	switch {
	case v.hi < w.hi || (v.hi == w.hi && v.lo < w.lo):
		return -1
	case v == w:
		return 0
	}
	return 1
}

func (v u128) trailingZeros() int {
	if v.lo != 0 {
		return bits.TrailingZeros64(v.lo)
	}
	if v.hi != 0 {
		return 64 + bits.TrailingZeros64(v.hi)
	}
	return 128
}

// plus is v + 2^k and whether it overflowed 128 bits.
func (v u128) plus(k int) (u128, bool) {
	var add u128
	if k >= 64 {
		add.hi = 1 << (k - 64)
	} else {
		add.lo = 1 << k
	}
	lo, c := bits.Add64(v.lo, add.lo, 0)
	hi, c2 := bits.Add64(v.hi, add.hi, c)
	return u128{hi, lo}, c2 != 0
}

// lastOf is the last address of the block of 2^k starting at v.
func (v u128) lastOf(k int) u128 {
	if k == 0 {
		return v
	}
	var m u128
	if k >= 64 {
		m.lo = ^uint64(0)
		if k == 128 {
			m.hi = ^uint64(0)
		} else {
			m.hi = 1<<(k-64) - 1
		}
	} else {
		m.lo = 1<<k - 1
	}
	return u128{v.hi | m.hi, v.lo | m.lo}
}

// RangePrefixes is the shortest list of prefixes covering exactly the
// addresses from lo to hi (one family, lo <= hi).
func RangePrefixes(lo, hi netip.Addr) []netip.Prefix {
	lo, hi = lo.Unmap(), hi.Unmap()
	if lo.Is4() != hi.Is4() || hi.Less(lo) {
		return nil
	}
	v4 := lo.Is4()
	width := 128
	if v4 {
		width = 32
	}
	cur, end := toU128(lo), toU128(hi)
	var out []netip.Prefix
	for {
		k := min(cur.trailingZeros(), width)
		for k > 0 && cur.lastOf(k).cmp(end) > 0 {
			k--
		}
		out = append(out, netip.PrefixFrom(fromU128(cur, v4), width-k))
		if cur.lastOf(k).cmp(end) >= 0 {
			return out
		}
		next, overflow := cur.plus(k)
		if overflow {
			return out
		}
		cur = next
	}
}

// CountryPrefixes reads DB-IP's country CSV and returns, for each code of
// want, the prefixes its ranges cover. A row that does not parse is an
// error: the file is the data a vhost is made of.
func CountryPrefixes(r io.Reader, want map[string]bool) (map[string][]netip.Prefix, error) {
	out := map[string][]netip.Prefix{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 4096)
	line, rows := 0, 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		f := strings.Split(text, ",")
		if len(f) != 3 {
			return nil, fmt.Errorf("country data, line %d: want start,end,country", line)
		}
		rows++
		cc := strings.Trim(f[2], `"`)
		if !want[cc] {
			continue
		}
		lo, err1 := netip.ParseAddr(strings.Trim(f[0], `"`))
		hi, err2 := netip.ParseAddr(strings.Trim(f[1], `"`))
		if err1 != nil || err2 != nil || lo.Is4() != hi.Is4() || hi.Less(lo) {
			return nil, fmt.Errorf("country data, line %d: not a range of addresses", line)
		}
		out[cc] = append(out[cc], RangePrefixes(lo, hi)...)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("country data: %w", err)
	}
	if rows < 1000 {
		return nil, fmt.Errorf("country data: %d rows, not the whole database", rows)
	}
	for cc := range out {
		slices.SortFunc(out[cc], func(a, b netip.Prefix) int { return a.Addr().Compare(b.Addr()) })
	}
	return out, nil
}
