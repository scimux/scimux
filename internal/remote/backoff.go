package remote

import (
	"sync"
	"time"
)

// Backoff is exponential backoff plus bounded jitter (FR-22).
type Backoff struct {
	mu       sync.Mutex
	cfg      BackoffConfig
	clock    Clock
	rng      RNG
	failures int
	up       bool
	upAt     time.Time
}

// NewBackoff stores configuration. It does not compute a delay.
func NewBackoff(cfg BackoffConfig, clock Clock, rng RNG) *Backoff {
	return &Backoff{cfg: cfg, clock: clock, rng: rng}
}

// Delay returns the next reconnect delay. RNG 0.5 is defined as zero jitter.
func (b *Backoff) Delay() (time.Duration, error) {
	if b == nil {
		return 0, ErrUnimplemented
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	base := b.baseLocked()
	j := 0.0
	if b.rng != nil && b.cfg.Jitter != 0 {
		j = (b.rng.Float64() - 0.5) * 2 * b.cfg.Jitter
	}
	d := time.Duration(float64(base) * (1 + j))
	if d < 0 {
		d = 0
	}
	if b.cfg.Max > 0 && d > b.cfg.Max {
		d = b.cfg.Max
	}
	return d, nil
}

func (b *Backoff) baseLocked() time.Duration {
	n := b.failures
	if n <= 0 {
		if b.cfg.Initial > 0 {
			return b.cfg.Initial
		}
		return 0
	}
	base := float64(b.cfg.Initial)
	for i := 1; i < n; i++ {
		base *= b.cfg.Factor
		if b.cfg.Max > 0 && time.Duration(base) >= b.cfg.Max {
			return b.cfg.Max
		}
	}
	d := time.Duration(base)
	if b.cfg.Max > 0 && d > b.cfg.Max {
		return b.cfg.Max
	}
	return d
}

// Failure records a reconnect failure.
func (b *Backoff) Failure() error {
	if b == nil {
		return ErrUnimplemented
	}
	b.mu.Lock()
	b.failures++
	b.up = false
	b.mu.Unlock()
	return nil
}

// NotifyUp records that a connection is up.
func (b *Backoff) NotifyUp() error {
	if b == nil {
		return ErrUnimplemented
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.up {
		b.up = true
		if b.clock != nil {
			b.upAt = b.clock.Now()
		}
	}
	return nil
}

// NotifyDown records that a connection was lost.
func (b *Backoff) NotifyDown() error {
	if b == nil {
		return ErrUnimplemented
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.up {
		hold := b.cfg.SuccessFor
		if hold == 0 {
			hold = b.cfg.SuccessFor
		}
		now := time.Time{}
		if b.clock != nil {
			now = b.clock.Now()
		}
		if !b.upAt.IsZero() && !now.IsZero() && now.Sub(b.upAt) >= hold {
			b.failures = 0
		}
	}
	b.up = false
	return nil
}

// Attempt is the current failure attempt count.
func (b *Backoff) Attempt() (int, error) {
	if b == nil {
		return 0, ErrUnimplemented
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.failures, nil
}
