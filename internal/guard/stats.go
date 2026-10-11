package guard

import (
	"math"
	"math/rand"
	"slices"
)

// quantile is the linearly interpolated q-quantile (0..1) of xs, which it
// sorts in place. It is the package's only percentile.
func quantile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	slices.Sort(xs)
	pos := q * float64(len(xs)-1)
	i := int(math.Floor(pos))
	if i >= len(xs)-1 {
		return xs[len(xs)-1]
	}
	return xs[i] + (pos-float64(i))*(xs[i+1]-xs[i])
}

// median of xs, leaving xs untouched.
func median(xs []float64) float64 {
	return quantile(slices.Clone(xs), 0.5)
}

// bootstrapResamples is enough for a stable 95% interval at n ≈ 100.
const bootstrapResamples = 2000

// medianInterval is the 95% percentile-bootstrap interval of the median.
func medianInterval(xs []float64, rng *rand.Rand) (lo, hi float64) {
	meds := make([]float64, bootstrapResamples)
	draw := make([]float64, len(xs))
	for i := range meds {
		for j := range draw {
			draw[j] = xs[rng.Intn(len(xs))]
		}
		meds[i] = quantile(draw, 0.5)
	}
	slices.Sort(meds)
	return quantile(meds, 0.025), quantile(meds, 0.975)
}

// share is part/whole in percent, 0 for an empty whole.
func share(part, whole int) float64 {
	if whole == 0 {
		return 0
	}
	return 100 * float64(part) / float64(whole)
}

// modal is the most common non-zero value, the smallest on a tie.
func modal(statuses []int) int {
	count := map[int]int{}
	best, bestN := 0, 0
	for _, s := range statuses {
		if s == 0 {
			continue
		}
		count[s]++
		if n := count[s]; n > bestN || (n == bestN && s < best) {
			best, bestN = s, n
		}
	}
	return best
}
