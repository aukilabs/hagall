package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aukilabs/hagall/pkg/admin"
	"github.com/aukilabs/hagall/pkg/config"
	"github.com/stretchr/testify/require"
)

func TestShutdownSignalIntent(t *testing.T) {
	require.Equal(t, admin.ShutdownIntentRestartSameIdentity, shutdownIntentForSignal(syscall.SIGTERM))
	require.Equal(t, admin.ShutdownIntentReassign, shutdownIntentForSignal(os.Interrupt))
}

type fakeRelayApplication struct {
	mu          sync.Mutex
	drainIntent admin.ShutdownIntent
	drainCalls  int
	drainErr    error
	closeErr    error
	closeCalls  int
	events      []string
	wait        chan struct{}
	waitEntered chan struct{}
	waitOnce    sync.Once
	closeCalled chan struct{}
	closeOnce   sync.Once
}

func (f *fakeRelayApplication) Wait() error {
	f.waitOnce.Do(func() {
		if f.waitEntered != nil {
			close(f.waitEntered)
		}
	})
	<-f.wait
	return nil
}

func (f *fakeRelayApplication) DrainForSignal(_ context.Context, intent admin.ShutdownIntent, _ time.Duration) (admin.DrainResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drainIntent = intent
	f.drainCalls++
	f.events = append(f.events, "drain:"+string(intent))
	return admin.DrainResult{ShutdownIntent: intent}, f.drainErr
}

func (f *fakeRelayApplication) Close(context.Context) error {
	f.mu.Lock()
	f.closeCalls++
	f.events = append(f.events, "close")
	closeErr := f.closeErr
	f.mu.Unlock()
	f.closeOnce.Do(func() {
		close(f.wait)
		if f.closeCalled != nil {
			close(f.closeCalled)
		}
	})
	return closeErr
}

func (f *fakeRelayApplication) shutdownSnapshot() (admin.ShutdownIntent, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.drainIntent, f.closeCalls
}

func (f *fakeRelayApplication) lifecycleSnapshot() (int, int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.drainCalls, f.closeCalls, append([]string(nil), f.events...)
}

func TestRunUsesSharedDrainStateMachineAndAlwaysCloses(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		signal     os.Signal
		wantIntent admin.ShutdownIntent
		drainErr   error
	}{
		{name: "SIGTERM restarts same identity", signal: syscall.SIGTERM, wantIntent: admin.ShutdownIntentRestartSameIdentity},
		{name: "SIGINT reassigns and preserves drain error", signal: os.Interrupt, wantIntent: admin.ShutdownIntentReassign, drainErr: errors.New("forced drain")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			application := &fakeRelayApplication{
				drainErr:    testCase.drainErr,
				wait:        make(chan struct{}),
				waitEntered: make(chan struct{}),
			}
			previous := startRelayApplication
			startRelayApplication = func(context.Context, config.Config, string, *slog.Logger) (relayApplication, error) {
				return application, nil
			}
			t.Cleanup(func() { startRelayApplication = previous })
			signals := make(chan os.Signal, 1)
			cfg := config.Defaults()
			cfg.ShutdownDrain = time.Second
			cfg.HTTP.RequestTimeout = time.Millisecond

			runResult := make(chan error, 1)
			go func() { runResult <- run(context.Background(), signals, cfg, "v0.0.0", slog.Default()) }()
			requireReceive(t, application.waitEntered, time.Second, "application did not finish startup")
			signals <- testCase.signal
			err := <-runResult
			require.ErrorIs(t, err, testCase.drainErr)
			intent, closeCalls := application.shutdownSnapshot()
			require.Equal(t, testCase.wantIntent, intent)
			require.Equal(t, 1, closeCalls)
		})
	}
}

func TestRunInterruptsBlockedStartupThenDrainsAndCloses(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		trigger func(chan<- os.Signal, context.CancelFunc)
	}{
		{
			name: "SIGTERM",
			trigger: func(signals chan<- os.Signal, _ context.CancelFunc) {
				signals <- syscall.SIGTERM
			},
		},
		{
			name: "parent cancellation",
			trigger: func(_ chan<- os.Signal, cancel context.CancelFunc) {
				cancel()
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			application := &fakeRelayApplication{wait: make(chan struct{})}
			startupEntered := make(chan struct{})
			startupCanceled := make(chan struct{})
			previous := startRelayApplication
			startRelayApplication = func(ctx context.Context, _ config.Config, _ string, _ *slog.Logger) (relayApplication, error) {
				close(startupEntered)
				<-ctx.Done()
				close(startupCanceled)
				return application, nil
			}
			t.Cleanup(func() { startRelayApplication = previous })

			parent, cancelParent := context.WithCancel(context.Background())
			defer cancelParent()
			signals := make(chan os.Signal, 1)
			cfg := config.Defaults()
			cfg.ShutdownDrain = time.Second
			cfg.HTTP.RequestTimeout = 20 * time.Millisecond
			runResult := make(chan error, 1)
			go func() { runResult <- run(parent, signals, cfg, "v0.0.0", slog.Default()) }()

			requireReceive(t, startupEntered, time.Second, "startup did not begin")
			testCase.trigger(signals, cancelParent)
			requireReceive(t, startupCanceled, time.Second, "startup context was not canceled")
			select {
			case err := <-runResult:
				require.NoError(t, err)
			case <-time.After(time.Second):
				t.Fatal("run waited indefinitely for canceled startup")
			}

			intent, closeCalls := application.shutdownSnapshot()
			require.Equal(t, admin.ShutdownIntentRestartSameIdentity, intent)
			require.Equal(t, 1, closeCalls)
		})
	}
}

func TestRunDrainsOwnedPostSessionStartupFailureBeforeClose(t *testing.T) {
	application := &fakeRelayApplication{wait: make(chan struct{})}
	providerSessionOpened := make(chan struct{})
	startupErr := errors.New("provider reconciliation interrupted after session open")
	previous := startRelayApplication
	startRelayApplication = func(ctx context.Context, _ config.Config, _ string, _ *slog.Logger) (relayApplication, error) {
		close(providerSessionOpened)
		<-ctx.Done()
		return application, errors.Join(startupErr, ctx.Err())
	}
	t.Cleanup(func() { startRelayApplication = previous })

	signals := make(chan os.Signal, 1)
	cfg := config.Defaults()
	cfg.ShutdownDrain = time.Second
	cfg.HTTP.RequestTimeout = 20 * time.Millisecond
	runResult := make(chan error, 1)
	go func() { runResult <- run(context.Background(), signals, cfg, "v0.0.0", slog.Default()) }()

	requireReceive(t, providerSessionOpened, time.Second, "provider session did not open")
	signals <- syscall.SIGTERM
	select {
	case err := <-runResult:
		require.ErrorIs(t, err, startupErr)
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("run did not stop after post-session startup cancellation")
	}
	drainCalls, closeCalls, events := application.lifecycleSnapshot()
	require.Equal(t, 1, drainCalls)
	require.Equal(t, 1, closeCalls)
	require.Equal(t, []string{"drain:restart_same_identity", "close"}, events)
}

func TestRunClosesOwnedApplicationOnOrdinaryStartupFailure(t *testing.T) {
	application := &fakeRelayApplication{wait: make(chan struct{})}
	startupErr := errors.New("ordinary startup failure")
	previous := startRelayApplication
	startRelayApplication = func(context.Context, config.Config, string, *slog.Logger) (relayApplication, error) {
		return application, startupErr
	}
	t.Cleanup(func() { startRelayApplication = previous })

	cfg := config.Defaults()
	cfg.HTTP.RequestTimeout = time.Millisecond
	err := run(context.Background(), make(chan os.Signal), cfg, "v0.0.0", slog.Default())
	require.ErrorIs(t, err, startupErr)
	drainCalls, closeCalls, events := application.lifecycleSnapshot()
	require.Zero(t, drainCalls)
	require.Equal(t, 1, closeCalls)
	require.Equal(t, []string{"close"}, events)
}

func TestRunBoundsUncooperativeStartupAndRetainsCleanupOwnership(t *testing.T) {
	application := &fakeRelayApplication{
		wait:        make(chan struct{}),
		closeCalled: make(chan struct{}),
	}
	startupEntered := make(chan context.Context, 1)
	releaseStartup := make(chan struct{})
	previous := startRelayApplication
	startRelayApplication = func(ctx context.Context, _ config.Config, _ string, _ *slog.Logger) (relayApplication, error) {
		startupEntered <- ctx
		<-releaseStartup
		return application, nil
	}
	t.Cleanup(func() { startRelayApplication = previous })

	signals := make(chan os.Signal, 1)
	cfg := config.Defaults()
	cfg.ShutdownDrain = time.Second
	cfg.HTTP.RequestTimeout = 10 * time.Millisecond
	runResult := make(chan error, 1)
	go func() { runResult <- run(context.Background(), signals, cfg, "v0.0.0", slog.Default()) }()

	var startupContext context.Context
	select {
	case startupContext = <-startupEntered:
	case <-time.After(time.Second):
		t.Fatal("startup did not begin")
	}
	signals <- syscall.SIGTERM
	requireReceive(t, startupContext.Done(), time.Second, "startup context was not canceled")
	select {
	case err := <-runResult:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(time.Second):
		t.Fatal("run waited indefinitely for startup that ignored cancellation")
	}

	close(releaseStartup)
	requireReceive(t, application.closeCalled, time.Second, "late application was not closed")
	intent, closeCalls := application.shutdownSnapshot()
	require.Equal(t, admin.ShutdownIntentRestartSameIdentity, intent)
	require.Equal(t, 1, closeCalls)
}

func requireReceive(t *testing.T, channel <-chan struct{}, timeout time.Duration, message string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(timeout):
		t.Fatal(message)
	}
}
