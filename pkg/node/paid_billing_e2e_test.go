package node

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	cryptorand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"math/big"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aukilabs/hagall/pkg/admission"
	"github.com/aukilabs/hagall/pkg/booking"
	"github.com/aukilabs/hagall/pkg/config"
	"github.com/aukilabs/hagall/pkg/dmsclient"
	"github.com/aukilabs/hagall/pkg/provider"
	"github.com/aukilabs/hagall/pkg/relayconfig"
	"github.com/aukilabs/hagall/pkg/verification"
	"github.com/google/uuid"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/stretchr/testify/require"
)

// Run only through the NCS local runner. DDS issues fixture identities; node,
// admission, signature verification, provider worker and DMS HTTP are real.
func TestPaidRelayTransportService(t *testing.T) {
	ready := os.Getenv("RELAY_TRANSPORT_HAGALL_READY")
	if ready == "" {
		t.Skip("opt-in cross-repository local fixture")
	}
	fixture := os.Getenv("RELAY_TRANSPORT_FIXTURE_URL")
	dms := os.Getenv("RELAY_TRANSPORT_DMS_URL")
	for _, address := range []string{fixture, dms} {
		u, err := url.Parse(address)
		require.NoError(t, err)
		require.Equal(t, "127.0.0.1", u.Hostname())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	var peers []string
	var stops []context.CancelFunc
	certificate, ca := paidTransportCertificate(t)
	require.NoError(t, os.WriteFile(ready+".ca.der", ca, 0600))
	var upgrades atomic.Int64
	for index := range 2 {
		id, stop := paidTransportProvider(t, ctx, fixture, dms, index, certificate, &upgrades)
		peers = append(peers, id)
		stops = append(stops, stop)
	}
	encoded, err := json.Marshal(peers)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(ready, encoded, 0600))
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatal("runner failed to stop relay fixture")
		case <-tick.C:
			// Public, nonsecret evidence that the native WSS clients actually
			// completed TLS + HTTP Upgrade through the ingress fixture.
			stats, err := json.Marshal(map[string]int64{"wss_upgrades": upgrades.Load()})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(ready+".tls.tmp", stats, 0600))
			require.NoError(t, os.Rename(ready+".tls.tmp", ready+".tls.json"))
			if _, err := os.Stat(ready + ".stop"); err == nil {
				return
			}
			if failed, err := os.ReadFile(ready + ".fail"); err == nil {
				for index, id := range peers {
					if string(failed) == id && stops[index] != nil {
						// Abrupt loss: stop transport and heartbeats without
						// reporting readiness changes or relinquishing in DMS.
						stops[index]()
						stops[index] = nil
						t.Log("cut active provider transport and control loops")
					}
				}
			}
		}
	}
}

func paidTransportProvider(t *testing.T, parent context.Context, fixture, dms string, index int, certificate tls.Certificate, upgrades *atomic.Int64) (string, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	options := testOptions(t, 8)
	id, err := peer.IDFromPrivateKey(options.Identity)
	require.NoError(t, err)
	var bases []string
	options.ListenAddresses = nil
	for _, suffix := range []string{"", "/ws"} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		port := listener.Addr().(*net.TCPAddr).Port
		require.NoError(t, listener.Close())
		options.ListenAddresses = append(options.ListenAddresses, ma.StringCast(fmt.Sprintf("/ip4/127.0.0.1/tcp/%d%s", port, suffix)))
		publicSuffix := ""
		if suffix != "" {
			publicSuffix = "/wss"
			// Hagall owns plaintext WS; production ingress terminates TLS.
			// The ephemeral local ingress keeps that boundary intact.
			upstream, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
			require.NoError(t, err)
			proxy := httputil.NewSingleHostReverseProxy(upstream)
			proxy.ModifyResponse = func(response *http.Response) error {
				if response.StatusCode == http.StatusSwitchingProtocols {
					upgrades.Add(1)
				}
				return nil
			}
			ingress := httptest.NewUnstartedServer(proxy)
			ingress.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
			ingress.StartTLS()
			t.Cleanup(ingress.Close)
			port = ingress.Listener.Addr().(*net.TCPAddr).Port
		}
		bases = append(bases, fmt.Sprintf("/dns4/relay-%d.interop.aukiverse.com/tcp/%d%s/p2p/%s", index, port, publicSuffix, id))
	}
	options.Advertised, err = relayconfig.Canonicalize(bases, id, options.Resources.RelayLimits)
	require.NoError(t, err)
	registry, err := booking.New(booking.Config{MaximumBookings: 8, MaximumAdmissions: 256, AdmissionTTL: 30 * time.Second})
	require.NoError(t, err)
	options.ACL = booking.NewACL(registry, nil)
	service, err := New(options)
	require.NoError(t, err)
	client := &http.Client{Timeout: 10 * time.Second}
	defaults := config.Defaults()
	ring, err := verification.NewRing(&verification.HTTPFetcher{URL: fixture + "/keys", Client: client}, verification.Options{
		ExpectedMethod: "ES256", PreviousKeyOverlap: defaults.VerificationKeys.PreviousKeyOverlap,
		MaxKeyStaleness: time.Hour, UnknownRefreshPeriod: 30 * time.Second,
	})
	require.NoError(t, err)
	require.NoError(t, ring.Preload(ctx))
	limiter, err := admission.New(admission.Config{TTL: admission.StreamIOTimeout, MaximumEntries: 128, Concurrency: 64, AttemptsPerPeer: 8, AttemptsPerIP: 32, AttemptWindow: 10 * time.Second})
	require.NoError(t, err)
	handler, err := admission.NewHandler(admission.HandlerOptions{Context: ctx, Limiter: limiter, Registry: registry, Verifier: ring, ClockSkew: time.Minute})
	require.NoError(t, err)
	service.Host().SetStreamHandler(admission.ProtocolID, handler.HandleStream)
	dmsClient, err := dmsclient.New(dmsclient.Options{Capacity: 8, BaseURL: dms, HTTPClient: client, AllowHTTPForTesting: true, StatusInterval: 30 * time.Second, MaxStatusBackoff: 10 * time.Second})
	require.NoError(t, err)
	response, err := client.Post(fixture+"/fixture/provider", "application/json", strings.NewReader(fmt.Sprintf(`{"index":%d,"peer_id":%q}`, index, id.String())))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	var identity struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&identity))
	require.NoError(t, response.Body.Close())
	metadata := dmsclient.Metadata{BaseAddresses: options.Advertised.Bases(), EndpointKeys: options.Advertised.EndpointKeys(), ConfigFingerprint: options.Advertised.Fingerprint(), Limits: dmsclient.Limits{DurationSeconds: 900, DataBytesPerDirection: 10 << 30}}
	expected := dmsclient.Expectations{Metadata: metadata, LocalCapacity: dmsclient.LocalCapacity{Total: 8, PerIP: 8, PerASN: 8}, SchedulingRevision: 1, NodeTokenExpiresAt: identity.ExpiresAt}
	opened, err := dmsClient.OpenSession(ctx, dmsclient.OpenInput{AccessToken: identity.Token, BootNonce: uuid.New(), Expected: expected})
	require.NoError(t, err)
	worker, err := provider.New(provider.Options{DMS: dmsClient, Registry: registry, Metadata: metadata, ClosePeer: service.Host().Network().ClosePeer, Config: provider.Config{
		InitialBackoff: time.Second, MaximumBackoff: 30 * time.Second, EmptyClaimMaxJitterFraction: 0.2,
		HeartbeatMinimumFraction: 0.25, HeartbeatMaximumFraction: 0.35, ExpirySweepInterval: time.Second, Random: rand.Float64, Now: time.Now,
	}})
	require.NoError(t, err)
	require.NoError(t, worker.SetControl(identity.Token, opened.Session))
	require.NoError(t, worker.Reconcile(ctx))
	status := dmsclient.ProviderStatus{AcceptingBookings: true}
	session, err := dmsClient.ReportStatus(ctx, identity.Token, opened.Session.ProviderSessionID, expected, status)
	require.NoError(t, err)
	require.NoError(t, worker.SetControl(identity.Token, *session))
	require.NoError(t, worker.Start(ctx))
	worker.SetAccepting(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case err := <-worker.Errors():
				if err != nil && ctx.Err() == nil {
					t.Errorf("provider worker failed: %v", err)
				}
				return
			case <-ticker.C:
				updated, err := dmsClient.ReportStatus(ctx, identity.Token, opened.Session.ProviderSessionID, expected, status)
				if ctx.Err() != nil {
					return
				}
				if err == nil {
					err = worker.SetControl(identity.Token, *updated)
				}
				if err != nil {
					t.Errorf("provider status failed: %v", err)
					return
				}
			}
		}
	}()
	stop := func() {
		cancel()
		_ = service.Close()
		<-done
		shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		require.NoError(t, worker.ShutdownPreserving(shutdown))
	}
	t.Cleanup(stop)
	return id.String(), stop
}

func paidTransportCertificate(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	require.NoError(t, err)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "disposable relay fixture CA"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(cryptorand.Reader, ca, ca, &key.PublicKey, key)
	require.NoError(t, err)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	require.NoError(t, err)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2),
		DNSNames:  []string{"relay-0.interop.aukiverse.com", "relay-1.interop.aukiverse.com"},
		NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(cryptorand.Reader, leaf, ca, &leafKey.PublicKey, key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey}, caDER
}
