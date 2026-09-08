// Package provider owns the standalone relay's bounded DMS booking worker.
package provider

import (
	"errors"
	"math"
	"time"
)

// RandomSource returns a value in [0,1). Production uses math/rand's
// concurrency-safe package function; tests inject exact values.
type RandomSource func() float64

func heartbeatDelay(now, leaseExpiresAt time.Time, minimumFraction, maximumFraction float64, random RandomSource) (time.Duration, error) {
	remaining := leaseExpiresAt.Sub(now)
	if remaining <= 0 {
		return 0, errors.New("provider lease is already expired")
	}
	if minimumFraction <= 0 || maximumFraction >= 1 || minimumFraction > maximumFraction {
		return 0, errors.New("provider heartbeat fractions are invalid")
	}
	fraction, err := randomFraction(minimumFraction, maximumFraction, random)
	if err != nil {
		return 0, err
	}
	delay := time.Duration(float64(remaining) * fraction)
	if delay <= 0 {
		delay = time.Nanosecond
	}
	if delay >= remaining {
		return 0, errors.New("provider heartbeat delay reaches the lease deadline")
	}
	return delay, nil
}

func emptyClaimDelay(base time.Duration, maximumJitterFraction float64, random RandomSource) (time.Duration, error) {
	if base <= 0 {
		return 0, errors.New("empty-claim delay must be positive")
	}
	if maximumJitterFraction < 0 || maximumJitterFraction > 0.20 {
		return 0, errors.New("empty-claim jitter must be in [0,0.20]")
	}
	fraction, err := randomFraction(0, maximumJitterFraction, random)
	if err != nil {
		return 0, err
	}
	delay := base + time.Duration(float64(base)*fraction)
	if delay < base {
		return 0, errors.New("empty-claim jitter overflowed")
	}
	return delay, nil
}

func randomFraction(minimum, maximum float64, random RandomSource) (float64, error) {
	if random == nil {
		return 0, errors.New("provider random source is required")
	}
	value := random()
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value >= 1 {
		return 0, errors.New("provider random source returned a value outside [0,1)")
	}
	return minimum + value*(maximum-minimum), nil
}

func nextBackoff(current, initial, maximum time.Duration) time.Duration {
	if initial <= 0 || maximum < initial {
		return 0
	}
	if current < initial {
		return initial
	}
	if current >= maximum || current > maximum/2 {
		return maximum
	}
	return current * 2
}
