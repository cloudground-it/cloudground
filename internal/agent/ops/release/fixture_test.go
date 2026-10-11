package release

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureKinds is the mix of a WordPress + WooCommerce tree by file count,
// with each kind's typical size: mostly PHP, then scripts, styles, images
// (already compressed) and the odd JSON, SVG and source map.
var fixtureKinds = []struct {
	ext    string
	share  int // per thousand files
	median int // bytes
	text   bool
}{
	{".php", 560, 4 << 10, true},
	{".js", 150, 8 << 10, true},
	{".css", 60, 6 << 10, true},
	{".json", 40, 2 << 10, true},
	{".svg", 40, 3 << 10, true},
	{".png", 80, 12 << 10, false},
	{".txt", 30, 3 << 10, true},
	{".map", 25, 24 << 10, true},
	{".woff2", 15, 30 << 10, false},
}

var fixtureWords = strings.Fields(`function return array string public static private
	class extends implements namespace use if else foreach while echo isset empty null true false
	this self parent const var let new wp_enqueue_script add_action add_filter apply_filters
	get_option update_option esc_html esc_attr __ _e WC_Product WC_Order woocommerce cart checkout
	color margin padding display flex none block width height px em rem rgba var(--wp--preset)`)

// writeFixture fills dir with a deterministic tree of about n files that
// looks like WordPress with WooCommerce to a copier and a compressor: the
// same mix of types and sizes, text that compresses about as well as code,
// directories a few levels deep. It returns the total size.
func writeFixture(tb testing.TB, dir string, n int) int64 {
	tb.Helper()
	rng := rand.New(rand.NewPCG(15, 2026))
	var dirs []string
	for _, top := range []string{"wp-admin", "wp-includes", "wp-content/plugins/woocommerce", "wp-content/themes/storefront"} {
		for i := range max(n/150, 1) {
			d := filepath.Join(dir, top, fmt.Sprintf("d%d", i%13), fmt.Sprintf("m%d", i))
			if i%3 == 0 {
				d = filepath.Join(d, "src")
			}
			dirs = append(dirs, d)
		}
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			tb.Fatal(err)
		}
	}
	var total int64
	buf := make([]byte, 0, 1<<20)
	for i := range n {
		pick, k := rng.IntN(1000), 0
		for k < len(fixtureKinds)-1 && pick >= fixtureKinds[k].share {
			pick -= fixtureKinds[k].share
			k++
		}
		kind := fixtureKinds[k]
		// Sizes spread from a quarter to four times the median, a few far
		// larger (bundled scripts).
		size := kind.median/4 + rng.IntN(kind.median*4)
		if rng.IntN(100) == 0 {
			size *= 20
		}
		buf = buf[:0]
		if kind.text {
			for len(buf) < size {
				buf = append(buf, fixtureWords[rng.IntN(len(fixtureWords))]...)
				switch rng.IntN(8) {
				case 0:
					buf = append(buf, ";\n\t"...)
				case 1:
					buf = fmt.Appendf(buf, "(%d)", rng.IntN(4096))
				default:
					buf = append(buf, ' ')
				}
			}
			buf = buf[:size]
		} else {
			buf = append(buf, make([]byte, size)...)
			for j := range buf {
				buf[j] = byte(rng.Uint32())
			}
		}
		p := filepath.Join(dirs[rng.IntN(len(dirs))], fmt.Sprintf("f%d%s", i, kind.ext))
		if err := os.WriteFile(p, buf, 0o644); err != nil {
			tb.Fatal(err)
		}
		total += int64(size)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.php"), []byte("<?php require __DIR__ . '/wp-blog-header.php';\n"), 0o644); err != nil {
		tb.Fatal(err)
	}
	return total
}
