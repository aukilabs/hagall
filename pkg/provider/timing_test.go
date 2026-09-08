package provider

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestHeartbeatDelayUsesReturnedRemainingLease(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	deadline := now.Add(100 * time.Second)

	minimum, err := heartbeatDelay(now, deadline, 0.25, 0.35, func() float64 { return 0 })
	require.NoError(t, err)
	require.Equal(t, 25*time.Second, minimum)
	maximumExclusive, err := heartbeatDelay(now, deadline, 0.25, 0.35, func() float64 { return math.Nextafter(1, 0) })
	require.NoError(t, err)
	require.GreaterOrEqual(t, maximumExclusive, 34*time.Second)
	require.LessOrEqual(t, maximumExclusive, 35*time.Second)

	_, err = heartbeatDelay(deadline, deadline, 0.25, 0.35, func() float64 { return 0 })
	require.ErrorContains(t, err, "expired")
}

func TestEmptyClaimDelayAddsOnlyPositiveBoundedJitter(t *testing.T) {
	base := 2 * time.Second
	minimum, err := emptyClaimDelay(base, 0.20, func() float64 { return 0 })
	require.NoError(t, err)
	require.Equal(t, base, minimum)
	maximumExclusive, err := emptyClaimDelay(base, 0.20, func() float64 { return math.Nextafter(1, 0) })
	require.NoError(t, err)
	require.GreaterOrEqual(t, maximumExclusive, base)
	require.LessOrEqual(t, maximumExclusive, 2400*time.Millisecond)
}

func TestProviderBackoffIsBounded(t *testing.T) {
	initial := time.Second
	maximum := 30 * time.Second
	require.Equal(t, initial, nextBackoff(0, initial, maximum))
	require.Equal(t, 2*time.Second, nextBackoff(initial, initial, maximum))
	require.Equal(t, maximum, nextBackoff(16*time.Second, initial, maximum))
	require.Equal(t, maximum, nextBackoff(maximum, initial, maximum))
	require.Zero(t, nextBackoff(0, 0, maximum))
}

func TestProviderTimingRejectsInvalidRandomSource(t *testing.T) {
	now := time.Now()
	_, err := heartbeatDelay(now, now.Add(time.Minute), 0.25, 0.35, func() float64 { return 1 })
	require.ErrorContains(t, err, "outside")
	_, err = emptyClaimDelay(time.Second, 0.21, func() float64 { return 0 })
	require.ErrorContains(t, err, "jitter")
}
