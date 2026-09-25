// Package admission contains bounded primitives for the authenticated relay
// stream, including its hard concurrency, time, and attempt-cache boundaries.
package admission

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

var (
	ErrBusy        = errors.New("relay authentication concurrency is exhausted")
	ErrRateLimited = errors.New("relay authentication attempt limit is exhausted")
	ErrCacheFull   = errors.New("relay authentication attempt cache is full")
)

type Config struct {
	TTL             time.Duration
	MaximumEntries  int
	Concurrency     int
	AttemptsPerPeer int
	AttemptsPerIP   int
	AttemptWindow   time.Duration
}

func (c Config) Validate() error {
	if c.TTL <= 0 || c.MaximumEntries < 2 || c.Concurrency <= 0 || c.AttemptsPerPeer <= 0 || c.AttemptsPerIP <= 0 || c.AttemptWindow <= 0 {
		return errors.New("relay authentication limits must be positive and cache capacity must be at least two")
	}
	return nil
}

type attemptBucket struct {
	started []time.Time
}

type Limiter struct {
	config Config
	slots  chan struct{}

	mu       sync.Mutex
	attempts map[string]attemptBucket
}

func New(config Config) (*Limiter, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Limiter{
		config:   config,
		slots:    make(chan struct{}, config.Concurrency),
		attempts: make(map[string]attemptBucket, config.MaximumEntries),
	}, nil
}

// Acquire admits one authenticated-stream attempt without waiting for a
// semaphore slot. The returned context is bounded by the configured total
// handshake lifetime; release must always be called.
func (l *Limiter) Acquire(parent context.Context, remotePeer peer.ID, remoteIP netip.Addr, now time.Time) (context.Context, func(), error) {
	if l == nil {
		return nil, nil, errors.New("relay authentication limiter is required")
	}
	if parent == nil || remotePeer == "" || !remoteIP.IsValid() {
		return nil, nil, errors.New("relay authentication attempt identity is invalid")
	}
	remoteIP = remoteIP.Unmap()
	select {
	case l.slots <- struct{}{}:
	default:
		return nil, nil, ErrBusy
	}
	if err := l.recordAttempt(remotePeer, remoteIP, now.UTC()); err != nil {
		<-l.slots
		return nil, nil, err
	}

	bounded, cancel := context.WithTimeout(parent, l.config.TTL)
	var once sync.Once
	release := func() {
		once.Do(func() {
			cancel()
			<-l.slots
		})
	}
	return bounded, release, nil
}

func (l *Limiter) recordAttempt(remotePeer peer.ID, remoteIP netip.Addr, now time.Time) error {
	peerKey := "peer:" + remotePeer.String()
	ipKey := "ip:" + remoteIP.String()
	cutoff := now.Add(-l.config.AttemptWindow)

	l.mu.Lock()
	defer l.mu.Unlock()
	for key, bucket := range l.attempts {
		bucket.started = retainAfter(bucket.started, cutoff)
		if len(bucket.started) == 0 {
			delete(l.attempts, key)
		} else {
			l.attempts[key] = bucket
		}
	}
	additional := 0
	if _, exists := l.attempts[peerKey]; !exists {
		additional++
	}
	if _, exists := l.attempts[ipKey]; !exists {
		additional++
	}
	if len(l.attempts)+additional > l.config.MaximumEntries {
		return ErrCacheFull
	}
	peerBucket := l.attempts[peerKey]
	ipBucket := l.attempts[ipKey]
	if len(peerBucket.started) >= l.config.AttemptsPerPeer || len(ipBucket.started) >= l.config.AttemptsPerIP {
		return ErrRateLimited
	}
	peerBucket.started = append(peerBucket.started, now)
	ipBucket.started = append(ipBucket.started, now)
	l.attempts[peerKey] = peerBucket
	l.attempts[ipKey] = ipBucket
	return nil
}

func retainAfter(values []time.Time, cutoff time.Time) []time.Time {
	first := 0
	for first < len(values) && !values[first].After(cutoff) {
		first++
	}
	if first == len(values) {
		return nil
	}
	return append(values[:0], values[first:]...)
}

func (l *Limiter) CachedEntries() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.attempts)
}
