package schedule

import "time"

// Backoff computes bounded exponential delays between retry attempts:
// base, 2·base, 4·base, … capped at max. A caller-optional Jitter hook adds
// extra delay so many apps failing at once do not re-synchronize; the
// scheduler injects its interval jitter here.
type Backoff struct {
	base, max time.Duration
	failures  int
	// Jitter, when set, receives the computed delay and returns extra delay
	// to add. May return zero.
	Jitter func(delay time.Duration) time.Duration
}

// NewBackoff returns a backoff over the given bounds. A non-positive base
// or max yields a zero delay (no backoff).
func NewBackoff(base, max time.Duration) *Backoff {
	if base <= 0 {
		base = 0
	}
	if max < base {
		max = base
	}
	return &Backoff{base: base, max: max}
}

// Next returns the delay before the next attempt and records the failure.
func (b *Backoff) Next() time.Duration {
	if b.base == 0 {
		b.failures++
		return 0
	}
	d := b.base
	for i := 0; i < b.failures && d < b.max; i++ {
		if d > b.max/2 { // cap before multiplying to avoid overflow
			d = b.max
			break
		}
		d *= 2
	}
	if d > b.max {
		d = b.max
	}
	b.failures++
	if b.Jitter != nil {
		d += b.Jitter(d)
	}
	return d
}

// Reset clears the failure count after a successful pass.
func (b *Backoff) Reset() { b.failures = 0 }

// Failures reports the current consecutive failure count.
func (b *Backoff) Failures() int { return b.failures }
