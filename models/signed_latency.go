package models

import (
	"crypto/ecdsa"
	"math"
	"sort"
	"time"

	"github.com/aukilabs/go-tooling/pkg/errors"
	"github.com/aukilabs/hagall-common/messages/hagallpb"
	hwebsocket "github.com/aukilabs/hagall-common/websocket"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type SignedLatency struct {
	RequestID     uint32
	StartedAt     time.Time
	Iteration     uint32
	PingRequests  map[uint32]LatencyMetricsData
	SessionID     string
	ClientID      string
	WalletAddress string

	sender     hwebsocket.ResponseSender
	privateKey *ecdsa.PrivateKey
}

type LatencyMetricsData struct {
	Start time.Time
	End   time.Time
}

func (s *SignedLatency) Start(privateKey *ecdsa.PrivateKey, sender hwebsocket.ResponseSender, requestID, iteration uint32, sessionID, clientID, walletAddress string) {
	s.StartedAt = time.Now()
	s.RequestID = requestID
	s.Iteration = iteration
	s.PingRequests = make(map[uint32]LatencyMetricsData, iteration)
	s.sender = sender
	s.SessionID = sessionID
	s.ClientID = clientID
	s.WalletAddress = walletAddress
	s.privateKey = privateKey

	s.sendPingRequest()
}

func (s *SignedLatency) OnPing(pingReqID uint32) error {
	pingRequest, ok := s.PingRequests[pingReqID]
	if !ok {
		return errors.New("ping request not found")
	}

	s.Iteration--
	s.PingRequests[pingReqID] = LatencyMetricsData{
		Start: pingRequest.Start,
		End:   time.Now(),
	}

	if s.Iteration > 0 {
		// Send new ping request
		s.sendPingRequest()
		return nil
	}

	// Compute metrics data and send to client
	var min, max, mean, p95, last float32
	var latencies []float32

	for _, v := range s.PingRequests {
		latency := float32(v.End.Sub(v.Start).Microseconds())
		latencies = append(latencies, latency)
		if latency < min || min == 0 {
			min = latency
		}
		if latency > max {
			max = latency
		}
		mean += latency
	}
	mean = float32(math.Round(float64(mean) / float64(len(s.PingRequests))))
	last = latencies[len(latencies)-1]

	sort.Slice(latencies, func(i, j int) bool {
		return latencies[i] < latencies[j]
	})

	index := int(float32(len(latencies)) * 0.95)
	if index < len(latencies) && index > 0 {
		p95 = latencies[index-1]
	}

	// create a list of ping request ids
	var pingRequestIDs []uint32
	for k := range s.PingRequests {
		pingRequestIDs = append(pingRequestIDs, k)
	}

	latencyData := &hagallpb.LatencyData{
		CreatedAt:      timestamppb.Now(),
		Min:            min,
		Max:            max,
		Mean:           mean,
		P95:            p95,
		Last:           last,
		IterationCount: uint32(len(s.PingRequests)),
		PingRequestIds: pingRequestIDs,
		SessionId:      s.SessionID,
		ClientId:       s.ClientID,
		WalletAddress:  s.WalletAddress,
	}

	data, err := proto.Marshal(latencyData)
	if err != nil {
		return errors.New("failed to marshal latency data").Wrap(err)
	}
	signature, err := crypto.Sign(crypto.Keccak256Hash(data).Bytes(), s.privateKey)
	if err != nil {
		return errors.New("failed to sign latency data").Wrap(err)
	}

	s.sender.Send(&hagallpb.SignedLatencyResponse{
		Type:      hagallpb.MsgType_MSG_TYPE_SIGNED_LATENCY_RESPONSE,
		Timestamp: timestamppb.Now(),
		RequestId: s.RequestID,
		Data:      latencyData,
		Signature: hexutil.Encode(signature),
	})
	return nil
}

func (s *SignedLatency) sendPingRequest() {
	pingReqID := uint32(time.Now().UnixNano())
	s.PingRequests[pingReqID] = LatencyMetricsData{
		Start: time.Now(),
	}
	s.sender.Send(&hagallpb.Response{
		Type:      hagallpb.MsgType_MSG_TYPE_PING_REQUEST,
		Timestamp: timestamppb.Now(),
		RequestId: pingReqID,
	})
}
