package admin

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/netutil"
)

func TestAdminAndMetricsListenersAreSeparateAndBounded(t *testing.T) {
	state := NewState()
	registry := prometheus.NewRegistry()
	require.NoError(t, state.RegisterMetrics(registry))
	options := testOptions(state, registry)
	servers, err := Start(options)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(t, servers.Close(ctx))
	})

	adminURL := "http://" + servers.AdminAddress()
	metricsURL := "http://" + servers.MetricsAddress()
	require.NotEqual(t, servers.AdminAddress(), servers.MetricsAddress())
	require.Equal(t, maxHeaderBytes, servers.adminServer.MaxHeaderBytes)
	require.Equal(t, maxHeaderBytes, servers.metricsServer.MaxHeaderBytes)
	require.Equal(t, 905*time.Second, servers.adminServer.WriteTimeout)
	require.Equal(t, 10*time.Second, servers.metricsServer.WriteTimeout)

	assertStatus(t, adminURL+"/livez", http.StatusOK)
	assertStatus(t, adminURL+"/readyz", http.StatusServiceUnavailable)
	assertStatus(t, adminURL+"/accepting", http.StatusServiceUnavailable)
	assertStatus(t, adminURL+"/metrics", http.StatusOK)
	assertStatus(t, adminURL+"/relay-info", http.StatusOK)
	assertStatus(t, metricsURL+"/livez", http.StatusNotFound)

	state.SetReady(true)
	assertStatus(t, adminURL+"/readyz", http.StatusOK)
	require.NoError(t, state.SetScheduling(AcceptingStateAccepting, 32, 1))
	assertStatus(t, adminURL+"/accepting", http.StatusOK)
	response, err := http.Get(metricsURL + "/metrics")
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "auki_relay_node_data_plane_ready 1")
}

func TestLimitedListenerBlocksAcceptUntilCapacityIsReleased(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	limited := netutil.LimitListener(listener, 1)
	t.Cleanup(func() { require.NoError(t, limited.Close()) })

	firstClient, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, firstClient.Close()) })
	firstServer, err := limited.Accept()
	require.NoError(t, err)

	secondClient, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, secondClient.Close()) })
	accepted := make(chan net.Conn, 1)
	acceptErrors := make(chan error, 1)
	go func() {
		connection, acceptErr := limited.Accept()
		if acceptErr != nil {
			acceptErrors <- acceptErr
			return
		}
		accepted <- connection
	}()

	select {
	case connection := <-accepted:
		_ = connection.Close()
		t.Fatal("second connection was accepted before listener capacity was released")
	case acceptErr := <-acceptErrors:
		t.Fatalf("accept second connection: %v", acceptErr)
	case <-time.After(50 * time.Millisecond):
	}

	require.NoError(t, firstServer.Close())
	select {
	case connection := <-accepted:
		require.NoError(t, connection.Close())
	case acceptErr := <-acceptErrors:
		t.Fatalf("accept second connection: %v", acceptErr)
	case <-time.After(time.Second):
		t.Fatal("second connection was not accepted after listener capacity was released")
	}
}

func TestClosingServersFailsBothProbesClosed(t *testing.T) {
	state := NewState()
	state.SetReady(true)
	registry := prometheus.NewRegistry()
	require.NoError(t, state.RegisterMetrics(registry))
	servers, err := Start(testOptions(state, registry))
	require.NoError(t, err)
	adminAddress := servers.AdminAddress()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, servers.Close(ctx))
	require.False(t, state.Alive())
	require.False(t, state.Ready())

	client := &http.Client{Timeout: 100 * time.Millisecond}
	_, err = client.Get("http://" + adminAddress + "/livez")
	require.Error(t, err)
}

func TestTimedOutServerCloseCanBeRetried(t *testing.T) {
	state := NewState()
	options := testOptions(state, prometheus.NewRegistry())
	started := make(chan struct{})
	release := make(chan struct{})
	options.Drain = func(context.Context, DrainRequest) (DrainResult, error) {
		close(started)
		<-release
		return DrainResult{Result: DrainOutcomeCompleted}, nil
	}
	servers, err := Start(options)
	require.NoError(t, err)

	request, err := http.NewRequest(
		http.MethodPost,
		"http://"+servers.AdminAddress()+"/drain",
		strings.NewReader(`{"shutdown_intent":"reassign","deadline_seconds":30}`),
	)
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	requestDone := make(chan error, 1)
	go func() {
		response, requestErr := http.DefaultClient.Do(request)
		if response != nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		requestDone <- requestErr
	}()
	<-started

	firstContext, firstCancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer firstCancel()
	err = servers.Close(firstContext)
	require.Error(t, err)
	require.True(t, errors.Is(err, context.DeadlineExceeded))
	close(release)
	require.NoError(t, <-requestDone)

	retryContext, retryCancel := context.WithTimeout(context.Background(), time.Second)
	defer retryCancel()
	require.NoError(t, servers.Close(retryContext))
	require.NoError(t, servers.Close(retryContext), "a completed close is idempotent")
}

func testOptions(state *State, registry *prometheus.Registry) Options {
	expiresAt := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	jwtExpiresAt := expiresAt.Add(time.Hour)
	return Options{
		Context:        context.Background(),
		AdminAddress:   "127.0.0.1:0",
		MetricsAddress: "127.0.0.1:0",
		State:          state,
		Registry:       registry,
		RelayInfo: func(context.Context) (RelayInfo, error) {
			return RelayInfo{
				PeerID:    "12D3KooWJ1FhJ7VLKfZWMsKJcxfwZNguvjGqNuGS2pCQ1mMfHySP",
				Addresses: []string{"/ip4/127.0.0.1/tcp/4001"},
				Limits: RelayLimits{
					DurationSeconds:       900,
					DataBytesPerDirection: 1 << 20,
				},
				DMS: DMSInfo{
					SessionPresent:           true,
					SessionExpiresAt:         &expiresAt,
					ProviderNodeJWTExpiresAt: &jwtExpiresAt,
					StatusTTLSeconds:         180,
					ProviderLeaseTTLSeconds:  180,
					EffectiveCapacity:        32,
					SchedulingRevision:       7,
					RecoveryGraceSeconds:     300,
				},
				Timers: TimerInfo{
					HTTPRequestTimeout:                    "10s",
					RetryInitialBackoff:                   "1s",
					RetryMaximumBackoff:                   "30s",
					ProviderStatusInterval:                "30s",
					ProviderStatusMaximumBackoff:          "10s",
					ConfiguredProviderSessionTTL:          "3m0s",
					NodeTokenRefreshFraction:              0.75,
					ProviderLeaseHeartbeatMinimumFraction: 0.25,
					ProviderLeaseHeartbeatMaximumFraction: 0.35,
					ReservationTTL:                        "10m0s",
					ShutdownDrain:                         "15m0s",
				},
			}, nil
		},
		Drain: func(context.Context, DrainRequest) (DrainResult, error) {
			return DrainResult{Result: DrainOutcomeCompleted}, nil
		},
	}
}

func assertStatus(t *testing.T, url string, expected int) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get(url)
	require.NoError(t, err)
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	require.Equal(t, expected, response.StatusCode)
}
