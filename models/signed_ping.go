package models

import (
	"github.com/aukilabs/go-tooling/pkg/logs"
	"github.com/aukilabs/hagall-common/messages/hagallpb"
	hwebsocket "github.com/aukilabs/hagall-common/websocket"
	"google.golang.org/protobuf/types/known/timestamppb"
	"sort"
	"time"
)

type SignedPing struct {
	RequestID    uint32
	StartedAt    time.Time
	Iteration    uint32
	PingRequests map[uint32]LatencyMetricsData
	SessionID    string
	ClientID     string
	Address      string

	sender hwebsocket.ResponseSender

	Min  float64
	Max  float64
	Mean float64
	P95  float64
}

type LatencyMetricsData struct {
	Start time.Time
	End   time.Time
}

func (s *SignedPing) Start(sender hwebsocket.ResponseSender, requestID, Iteration uint32, SessionId, ClientId, Address string) {
	s.StartedAt = time.Now()
	s.RequestID = requestID
	s.Iteration = Iteration
	s.PingRequests = make(map[uint32]LatencyMetricsData, Iteration)
	s.sender = sender
	s.SessionID = SessionId
	s.ClientID = ClientId
	s.Address = Address

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

func (s *SignedPing) onPing(pingReqID uint32) {
	if _, ok := s.PingRequests[pingReqID]; !ok {
		logs.Warn("ping request not found")
		return
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
		return
	}

	// Compute metrics data and send to client
	var min, max, mean, p95 float64
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

	s.sender.Send(&hagallpb.SignedPingResponse{
		Type:      hagallpb.MsgType_MSG_TYPE_SIGNED_PING_RESPONSE,
		Timestamp: timestamppb.Now(),
		RequestId: s.RequestID,
		Data: &hagallpb.LatencyData{
			Min:       min,
			Max:       max,
			Mean:      mean,
			P95:       p95,
			SessionId: s.SessionID,
			ClientId:  s.ClientID,
			Address:   s.Address,
		},
		Sig: "todo",
	})
}
