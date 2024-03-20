package models

import (
	"crypto/ecdsa"
	"github.com/aukilabs/go-tooling/pkg/errors"
	"github.com/aukilabs/hagall-common/messages/hagallpb"
	hwebsocket "github.com/aukilabs/hagall-common/websocket"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"sort"
	"time"
)

type SignedLatency struct {
	RequestID    uint32
	StartedAt    time.Time
	Iteration    uint32
	PingRequests map[uint32]LatencyMetricsData
	SessionID    string
	ClientID     string
	Address      string

	sender     hwebsocket.ResponseSender
	privateKey *ecdsa.PrivateKey

	Min  float64
	Max  float64
	Mean float64
	P95  float64
}

type LatencyMetricsData struct {
	Start time.Time
	End   time.Time
}

func (s *SignedLatency) Start(privateKey *ecdsa.PrivateKey, sender hwebsocket.ResponseSender, requestID, Iteration uint32, SessionId, ClientId, Address string) {
	s.StartedAt = time.Now()
	s.RequestID = requestID
	s.Iteration = Iteration
	s.PingRequests = make(map[uint32]LatencyMetricsData, Iteration)
	s.sender = sender
	s.SessionID = SessionId
	s.ClientID = ClientId
	s.Address = Address
	s.privateKey = privateKey

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

func (s *SignedLatency) onPing(pingReqID uint32) error {
	if _, ok := s.PingRequests[pingReqID]; !ok {
		return errors.New("ping request not found")
	}

	s.Iteration--
	s.PingRequests[pingReqID] = LatencyMetricsData{
		Start: s.PingRequests[pingReqID].Start,
		End:   time.Now(),
	}

	if s.Iteration > 0 {
		// Send new ping request
		pingReqID := uint32(time.Now().UnixNano())
		s.PingRequests[pingReqID] = LatencyMetricsData{
			Start: time.Now(),
		}
		s.sender.Send(&hagallpb.Response{
			Type:      hagallpb.MsgType_MSG_TYPE_PING_REQUEST,
			Timestamp: timestamppb.Now(),
			RequestId: pingReqID,
		})
		return nil
	}

	// Compute metrics data and send to client
	var min, max, mean, p95, last float64
	var latencies []float64

	for _, v := range s.PingRequests {
		latency := v.End.Sub(v.Start).Seconds()
		latencies = append(latencies, latency)
		if latency < min || min == 0 {
			min = latency
		}
		if latency > max {
			max = latency
		}
		mean += latency
	}
	mean = mean / float64(len(s.PingRequests))

	sort.Float64s(latencies)
	index := int(float64(len(latencies)) * 0.95)
	if index < len(latencies) && index > 0 {
		p95 = latencies[index-1]
	}

	last = latencies[len(latencies)-1]

	// create a list of ping request ids
	var pingRequestIds []uint32
	for k := range s.PingRequests {
		pingRequestIds = append(pingRequestIds, k)
	}

	latencyData := &hagallpb.LatencyData{
		CreatedAt:      timestamppb.Now(),
		Min:            min,
		Max:            max,
		Mean:           mean,
		P95:            p95,
		Last:           last,
		IterationCount: uint32(len(s.PingRequests)),
		PingRequestIds: pingRequestIds,
		SessionId:      s.SessionID,
		ClientId:       s.ClientID,
		WalletAddress:  s.Address,
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
