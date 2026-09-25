package admission

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aukilabs/hagall/pkg/authpkg"
	"github.com/aukilabs/hagall/pkg/booking"
	"github.com/aukilabs/hagall/pkg/telemetry"
	"github.com/google/uuid"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/p2p/security/noise"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
)

const (
	ProtocolID           protocol.ID = "/auki-p2p/relay-auth/1"
	ServiceName                      = "auki.relay-auth/v1"
	RequestMaximumBytes              = 64 << 10
	ResponseMaximumBytes             = 4 << 10
	StreamIOTimeout                  = 10 * time.Second
)

var deniedResponse = []byte(`{"accepted":false}`)

type P2PVerifier interface {
	VerifyP2P(context.Context, string, time.Time, time.Duration) (*authpkg.P2PAccessTokenClaims, time.Time, error)
}

type HandlerOptions struct {
	Context   context.Context
	Limiter   *Limiter
	Registry  *booking.Registry
	Verifier  P2PVerifier
	ClockSkew time.Duration
	Now       func() time.Time
	Observer  interface {
		ObserveSourceAuth(telemetry.SourceAuthOutcome)
	}
}

type Handler struct {
	context   context.Context
	limiter   *Limiter
	registry  *booking.Registry
	verifier  P2PVerifier
	clockSkew time.Duration
	now       func() time.Time
	observer  interface {
		ObserveSourceAuth(telemetry.SourceAuthOutcome)
	}
}

type Request struct {
	Version        int
	DomainID       uuid.UUID
	TargetPeerID   peer.ID
	P2PAccessToken string
}

type response struct {
	Accepted      bool   `json:"accepted"`
	AcceptedUntil string `json:"accepted_until,omitempty"`
}

func NewHandler(options HandlerOptions) (*Handler, error) {
	if options.Limiter == nil || options.Registry == nil || options.Verifier == nil {
		return nil, errors.New("relay auth limiter, booking registry, and P2P verifier are required")
	}
	if options.ClockSkew < 0 {
		return nil, errors.New("relay auth verification clock skew cannot be negative")
	}
	parent := options.Context
	if parent == nil {
		parent = context.Background()
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Handler{
		context:   parent,
		limiter:   options.Limiter,
		registry:  options.Registry,
		verifier:  options.Verifier,
		clockSkew: options.ClockSkew,
		now:       now,
		observer:  options.Observer,
	}, nil
}

// SetObserver installs private metrics before the handler is registered on the
// libp2p host. Callers must not replace it after serving starts.
func (h *Handler) SetObserver(observer interface {
	ObserveSourceAuth(telemetry.SourceAuthOutcome)
}) {
	if h != nil {
		h.observer = observer
	}
}

// HandleStream handles exactly one framed request and one framed response. All
// authorization failures use the same response and never include the token.
func (h *Handler) HandleStream(stream network.Stream) {
	if h == nil || stream == nil {
		return
	}
	defer stream.Close()
	if err := stream.Scope().SetService(ServiceName); err != nil {
		_ = stream.ResetWithError(network.StreamResourceLimitExceeded)
		return
	}

	source, remoteIP, err := directRemoteIdentity(stream)
	if err != nil {
		h.deny(stream, telemetry.SourceAuthInvalidPeer)
		return
	}
	bounded, release, err := h.limiter.Acquire(h.context, source, remoteIP, h.now().UTC())
	if err != nil {
		outcome := telemetry.SourceAuthInvalidPeer
		switch {
		case errors.Is(err, ErrRateLimited):
			outcome = telemetry.SourceAuthRateLimited
		case errors.Is(err, ErrBusy):
			outcome = telemetry.SourceAuthBusy
		case errors.Is(err, ErrCacheFull):
			outcome = telemetry.SourceAuthCacheFull
		}
		h.deny(stream, outcome)
		return
	}
	defer release()
	if err := bounded.Err(); err != nil {
		h.deny(stream, telemetry.SourceAuthContextDone)
		return
	}

	request, reserved, err := readRequest(stream)
	if reserved > 0 {
		defer stream.Scope().ReleaseMemory(reserved)
	}
	if err != nil {
		h.deny(stream, telemetry.SourceAuthMalformed)
		return
	}
	snapshot, found := h.registry.SnapshotForAdmission(request.TargetPeerID, request.DomainID)
	if !found {
		h.deny(stream, telemetry.SourceAuthUnbooked)
		return
	}

	verificationNow := h.now().UTC()
	claims, literalExpiresAt, err := h.verifier.VerifyP2P(bounded, request.P2PAccessToken, verificationNow, h.clockSkew)
	if err != nil || claims == nil {
		h.deny(stream, telemetry.SourceAuthInvalidToken)
		return
	}
	commitNow := h.now().UTC()
	if !commitNow.Before(literalExpiresAt.UTC()) || !claimsBindSourceAndDomain(claims, source, request.DomainID) {
		h.deny(stream, telemetry.SourceAuthIdentityDenied)
		return
	}
	acceptedUntil, err := h.registry.Admit(booking.AdmissionRequest{
		SourcePeerID:        source,
		DomainID:            request.DomainID,
		TargetPeerID:        request.TargetPeerID,
		ExpectedFence:       snapshot.Authority.Fence,
		LiteralJWTExpiresAt: literalExpiresAt,
	})
	if err != nil {
		outcome := telemetry.SourceAuthUnbooked
		if errors.Is(err, booking.ErrAdmissionCapacity) {
			outcome = telemetry.SourceAuthCapacityDenied
		}
		h.deny(stream, outcome)
		return
	}
	if !h.now().UTC().Before(acceptedUntil) {
		h.deny(stream, telemetry.SourceAuthInvalidToken)
		return
	}
	payload, err := json.Marshal(response{
		Accepted:      true,
		AcceptedUntil: acceptedUntil.UTC().Format(time.RFC3339Nano),
	})
	if err != nil || len(payload) > ResponseMaximumBytes || writeFrame(stream, payload) != nil {
		h.observe(telemetry.SourceAuthWriteFailed)
		_ = stream.Reset()
		return
	}
	h.observe(telemetry.SourceAuthAccepted)
}

func (h *Handler) deny(stream network.Stream, outcome telemetry.SourceAuthOutcome) {
	h.observe(outcome)
	if err := writeFrame(stream, deniedResponse); err != nil {
		h.observe(telemetry.SourceAuthWriteFailed)
		_ = stream.Reset()
	}
}

func (h *Handler) observe(outcome telemetry.SourceAuthOutcome) {
	if h != nil && h.observer != nil {
		h.observer.ObserveSourceAuth(outcome)
	}
}

func directRemoteIdentity(stream network.Stream) (peer.ID, netip.Addr, error) {
	connection := stream.Conn()
	if connection == nil || connection.RemotePeer() == "" {
		return "", netip.Addr{}, errors.New("authenticated remote peer is required")
	}
	if connection.ConnState().Security != noise.ID {
		return "", netip.Addr{}, errors.New("relay auth requires Noise security")
	}
	if connection.Stat().Limited {
		return "", netip.Addr{}, errors.New("relay auth requires a direct connection")
	}
	remoteAddress := connection.RemoteMultiaddr()
	if remoteAddress == nil {
		return "", netip.Addr{}, errors.New("remote multiaddr is required")
	}
	if _, err := remoteAddress.ValueForProtocol(ma.P_CIRCUIT); err == nil {
		return "", netip.Addr{}, errors.New("relay auth requires a direct connection")
	}
	ip, err := manet.ToIP(remoteAddress)
	if err != nil {
		return "", netip.Addr{}, errors.New("remote IP is required")
	}
	remoteIP, valid := netip.AddrFromSlice(ip)
	if !valid {
		return "", netip.Addr{}, errors.New("remote IP is invalid")
	}
	return connection.RemotePeer(), remoteIP.Unmap(), nil
}

func readRequest(stream network.Stream) (Request, int, error) {
	if err := stream.SetReadDeadline(time.Now().Add(StreamIOTimeout)); err != nil {
		return Request{}, 0, err
	}
	var header [4]byte
	if _, err := io.ReadFull(stream, header[:]); err != nil {
		return Request{}, 0, err
	}
	length := binary.BigEndian.Uint32(header[:])
	if length == 0 || length > RequestMaximumBytes {
		return Request{}, 0, errors.New("relay auth request frame length is invalid")
	}
	reserved := int(length) + ResponseMaximumBytes
	if err := stream.Scope().ReserveMemory(reserved, network.ReservationPriorityAlways); err != nil {
		return Request{}, 0, err
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(stream, payload); err != nil {
		return Request{}, reserved, err
	}
	request, err := decodeRequest(payload)
	return request, reserved, err
}

func decodeRequest(payload []byte) (Request, error) {
	if !utf8.Valid(payload) {
		return Request{}, errors.New("relay auth request must be UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return Request{}, errors.New("relay auth request must be one JSON object")
	}
	fields := make(map[string]json.RawMessage, 4)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return Request{}, errors.New("relay auth request contains an invalid field")
		}
		key, ok := keyToken.(string)
		if !ok {
			return Request{}, errors.New("relay auth request field name is invalid")
		}
		switch key {
		case "version", "domain_id", "target_peer_id", "p2p_access_token":
		default:
			return Request{}, fmt.Errorf("relay auth request contains unknown field %q", key)
		}
		if _, duplicate := fields[key]; duplicate {
			return Request{}, fmt.Errorf("relay auth request contains duplicate field %q", key)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return Request{}, errors.New("relay auth request contains an invalid field value")
		}
		fields[key] = value
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || decoder.InputOffset() != int64(len(payload)) {
		return Request{}, errors.New("relay auth request contains trailing bytes")
	}
	if len(fields) != 4 {
		return Request{}, errors.New("relay auth request is missing a required field")
	}

	var version int
	var domainValue, targetValue, token string
	if json.Unmarshal(fields["version"], &version) != nil || version != 1 {
		return Request{}, errors.New("relay auth request version is unsupported")
	}
	if json.Unmarshal(fields["domain_id"], &domainValue) != nil || domainValue == "" || strings.TrimSpace(domainValue) != domainValue {
		return Request{}, errors.New("relay auth request Domain is invalid")
	}
	domain, err := uuid.Parse(domainValue)
	if err != nil {
		return Request{}, errors.New("relay auth request Domain is invalid")
	}
	if json.Unmarshal(fields["target_peer_id"], &targetValue) != nil || targetValue == "" || strings.TrimSpace(targetValue) != targetValue {
		return Request{}, errors.New("relay auth request target is invalid")
	}
	target, err := peer.Decode(targetValue)
	if err != nil {
		return Request{}, errors.New("relay auth request target is invalid")
	}
	if json.Unmarshal(fields["p2p_access_token"], &token) != nil || token == "" || strings.TrimSpace(token) != token {
		return Request{}, errors.New("relay auth request token is invalid")
	}
	return Request{Version: version, DomainID: domain, TargetPeerID: target, P2PAccessToken: token}, nil
}

func claimsBindSourceAndDomain(claims *authpkg.P2PAccessTokenClaims, source peer.ID, domain uuid.UUID) bool {
	claimedPeer, err := peer.Decode(claims.PeerID)
	if err != nil || claimedPeer != source {
		return false
	}
	for _, value := range claims.DomainIDs {
		claimedDomain, err := uuid.Parse(value)
		if err == nil && claimedDomain == domain {
			return true
		}
	}
	return false
}

func writeFrame(stream network.Stream, payload []byte) error {
	if len(payload) == 0 || len(payload) > ResponseMaximumBytes {
		return errors.New("relay auth response frame length is invalid")
	}
	if err := stream.SetWriteDeadline(time.Now().Add(StreamIOTimeout)); err != nil {
		return err
	}
	frame := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(payload)))
	copy(frame[4:], payload)
	for len(frame) > 0 {
		written, err := stream.Write(frame)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		frame = frame[written:]
	}
	return nil
}
