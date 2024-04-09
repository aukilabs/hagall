package websocket

import (
	"context"
	"github.com/aukilabs/hagall/modules"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"google.golang.org/protobuf/proto"
	"testing"
	"time"

	"github.com/aukilabs/hagall-common/messages/hagallpb"
	"github.com/aukilabs/hagall-common/scenario"
	hwebsocket "github.com/aukilabs/hagall-common/websocket"
	"github.com/aukilabs/hagall/featureflag"
	"github.com/aukilabs/hagall/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestHanslerSendSyncClock(t *testing.T) {
	clientA, _, close := NewTestingEnv(t, newTestHandler())
	defer close()

	err := scenario.NewScenario(clientA).
		Receive(scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_SYNC_CLOCK), func(msg hwebsocket.Msg) error {
			var res hagallpb.SyncClock
			err := msg.DataTo(&res)

			require.NoError(t, err)
			require.NotZero(t, msg.Time)
			return err
		}).
		Run(context.Background())
	require.NoError(t, err)
}

func TestHandlerHandlePing(t *testing.T) {
	clientA, _, close := NewTestingEnv(t, newTestHandler())
	defer close()

	err := scenario.NewScenario(clientA).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.Request{
				Type:      hagallpb.MsgType_MSG_TYPE_PING_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 1,
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PING_RESPONSE),
			scenario.FilterByRequestID(1),
		).
		Run(context.Background())
	require.NoError(t, err)
}

func TestHandlerHandleSignedLatency(t *testing.T) {
	sessionStore := &models.SessionStore{
		DiscoveryService: &testClient{},
	}
	privateKey, err := crypto.GenerateKey()
	require.NoError(t, err)
	clientA, _, close := NewTestingEnv(t, func() Handler {
		m := make([]modules.Module, 0)
		var h Handler = &RealtimeHandler{
			ClientSyncClockInterval: time.Millisecond * 250,
			ClientIdleTimeout:       time.Minute,
			FrameDuration:           time.Millisecond * 50,
			Sessions:                sessionStore,
			Modules:                 m,
			PrivateKey:              privateKey,
		}

		h = HandlerWithLogs(h, time.Millisecond*100)
		h = HandlerWithMetrics(h, "https://auki-test.com")
		return h
	})
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var tmpRequestID uint32

	err = scenario.NewScenario(clientA).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 1,
			}
		}).
		Receive(scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE), func(msg hwebsocket.Msg) error {
			var res hagallpb.ParticipantJoinResponse
			err := msg.DataTo(&res)
			require.NoError(t, err)
			return err
		}).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.SignedLatencyRequest{
				Type:           hagallpb.MsgType_MSG_TYPE_SIGNED_LATENCY_REQUEST,
				Timestamp:      timestamppb.Now(),
				RequestId:      2,
				IterationCount: 3,
				WalletAddress:  "0x123456789",
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PING_REQUEST),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.Response
				err := msg.DataTo(&res)
				require.NoError(t, err)
				tmpRequestID = res.RequestId
				return err
			},
		).
		Send(func() hwebsocket.ProtoMsg {
			time.Sleep(time.Duration(3) * time.Millisecond)
			return &hagallpb.Response{
				Type:      hagallpb.MsgType_MSG_TYPE_PING_RESPONSE,
				Timestamp: timestamppb.Now(),
				RequestId: tmpRequestID,
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PING_REQUEST),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.Response
				err := msg.DataTo(&res)
				require.NoError(t, err)
				tmpRequestID = res.RequestId
				return err
			},
		).
		Send(func() hwebsocket.ProtoMsg {
			time.Sleep(time.Duration(1) * time.Millisecond)
			return &hagallpb.Response{
				Type:      hagallpb.MsgType_MSG_TYPE_PING_RESPONSE,
				Timestamp: timestamppb.Now(),
				RequestId: tmpRequestID,
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PING_REQUEST),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.Response
				err := msg.DataTo(&res)
				require.NoError(t, err)
				tmpRequestID = res.RequestId
				return err
			},
		).
		Send(func() hwebsocket.ProtoMsg {
			time.Sleep(time.Duration(2) * time.Millisecond)
			return &hagallpb.Response{
				Type:      hagallpb.MsgType_MSG_TYPE_PING_RESPONSE,
				Timestamp: timestamppb.Now(),
				RequestId: tmpRequestID,
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_SIGNED_LATENCY_RESPONSE),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.SignedLatencyResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)
				require.Equal(t, uint32(3), res.Data.IterationCount)
				require.Equal(t, "0x123456789", res.Data.WalletAddress)

				t.Log("res.Data.Max: ", res.Data.Max)
				t.Log("res.Data.Min: ", res.Data.Min)
				t.Log("res.Data.P95: ", res.Data.P95)
				t.Log("res.Data.Mean: ", res.Data.Mean)
				t.Log("res.Data.Last: ", res.Data.Last)

				require.GreaterOrEqual(t, res.Data.Max, float32(3000))
				require.LessOrEqual(t, res.Data.Min, float32(2000))
				require.InDelta(t, float32(2500), res.Data.Mean, 500)
				require.InDelta(t, float32(2500), res.Data.P95, 1000)
				require.InDelta(t, float32(2500), res.Data.Last, 1000)

				data, err := proto.Marshal(res.Data)

				publicKeyECDSA, err := crypto.SigToPub(crypto.Keccak256Hash(data).Bytes(), common.FromHex(res.Signature))
				require.NoError(t, err)

				addr := crypto.PubkeyToAddress(*publicKeyECDSA).Hex()
				publicKey := crypto.PubkeyToAddress(privateKey.PublicKey).Hex()

				require.Equal(t, addr, publicKey)

				return err
			},
		).
		Run(ctx)
	require.NoError(t, err)
}

func TestHandlerHandleSignedLatencyWithoutJoining(t *testing.T) {
	clientA, _, close := NewTestingEnv(t, newTestHandler())
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := scenario.NewScenario(clientA).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.SignedLatencyRequest{
				Type:           hagallpb.MsgType_MSG_TYPE_SIGNED_LATENCY_REQUEST,
				Timestamp:      timestamppb.Now(),
				RequestId:      1,
				IterationCount: 10,
				WalletAddress:  "0x123456789",
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
			scenario.FilterByRequestID(1),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ErrorResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_UNAUTHORIZED, res.Code)
				return err
			}).
		Run(ctx)
	require.NoError(t, err)
}

func TestHandlerHandleSignedLatencyWithSmallIteration(t *testing.T) {
	clientA, _, close := NewTestingEnv(t, newTestHandler())
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := scenario.NewScenario(clientA).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 1,
			}
		}).
		Receive(scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE), func(msg hwebsocket.Msg) error {
			var res hagallpb.ParticipantJoinResponse
			err := msg.DataTo(&res)
			require.NoError(t, err)
			return err
		}).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.SignedLatencyRequest{
				Type:           hagallpb.MsgType_MSG_TYPE_SIGNED_LATENCY_REQUEST,
				Timestamp:      timestamppb.Now(),
				RequestId:      2,
				IterationCount: 1,
				WalletAddress:  "0x123456789",
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
			scenario.FilterByRequestID(2),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ErrorResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_BAD_REQUEST, res.Code)
				return err
			}).
		Run(ctx)
	require.NoError(t, err)
}
func TestHandlerHandleSignedLatencyWithBigIteration(t *testing.T) {
	clientA, _, close := NewTestingEnv(t, newTestHandler())
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := scenario.NewScenario(clientA).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 1,
			}
		}).
		Receive(scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE), func(msg hwebsocket.Msg) error {
			var res hagallpb.ParticipantJoinResponse
			err := msg.DataTo(&res)
			require.NoError(t, err)
			return err
		}).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.SignedLatencyRequest{
				Type:           hagallpb.MsgType_MSG_TYPE_SIGNED_LATENCY_REQUEST,
				Timestamp:      timestamppb.Now(),
				RequestId:      2,
				IterationCount: 100,
				WalletAddress:  "0x123456789",
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
			scenario.FilterByRequestID(2),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ErrorResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_BAD_REQUEST, res.Code)
				return err
			}).
		Run(ctx)
	require.NoError(t, err)
}

func TestHandlerHandleSignedLatencyWithoutWallet(t *testing.T) {
	clientA, _, close := NewTestingEnv(t, newTestHandler())
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := scenario.NewScenario(clientA).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 1,
			}
		}).
		Receive(scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE), func(msg hwebsocket.Msg) error {
			var res hagallpb.ParticipantJoinResponse
			err := msg.DataTo(&res)
			require.NoError(t, err)
			return err
		}).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.SignedLatencyRequest{
				Type:           hagallpb.MsgType_MSG_TYPE_SIGNED_LATENCY_REQUEST,
				Timestamp:      timestamppb.Now(),
				RequestId:      2,
				IterationCount: 10,
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
			scenario.FilterByRequestID(2),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ErrorResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_BAD_REQUEST, res.Code)
				return err
			}).
		Run(ctx)
	require.NoError(t, err)
}

func TestHandlerHandleParticipantJoin(t *testing.T) {
	clientA, clientB, close := NewTestingEnv(t, newTestHandler())
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var sessionID string
	var participantA models.Participant
	var participantB models.Participant

	err := scenario.NewScenario(clientA).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 1,
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			scenario.FilterByRequestID(1),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ParticipantJoinResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				require.NotZero(t, res.Timestamp)
				require.NotEmpty(t, res.SessionId)
				require.NotZero(t, res.ParticipantId)

				sessionID = res.SessionId
				participantA.ID = res.ParticipantId
				return err
			}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_SESSION_STATE),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.SessionState
				err := msg.DataTo(&res)
				require.NoError(t, err)

				require.NotZero(t, res.Timestamp)
				require.Len(t, res.Participants, 1)
				require.Equal(t, participantA.ID, res.Participants[0].Id)
				require.Empty(t, res.Entities)
				return err
			}).
		Run(ctx)
	require.NoError(t, err)

	joinBOriginTime := time.Now()

	err = scenario.NewScenario(clientB).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.New(joinBOriginTime),
				RequestId: 2,
				SessionId: sessionID,
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			scenario.FilterByRequestID(2),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ParticipantJoinResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				participantB.ID = res.ParticipantId
				return err
			}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_SESSION_STATE),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.SessionState
				err := msg.DataTo(&res)
				require.NoError(t, err)

				require.NotZero(t, res.Timestamp)
				require.Len(t, res.Participants, 2)
				require.Empty(t, res.Entities)
				return err
			}).
		Run(ctx)
	require.NoError(t, err)

	err = scenario.NewScenario(clientA).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_BROADCAST),
			func(msg hwebsocket.Msg) error {
				var bc hagallpb.ParticipantJoinBroadcast
				err := msg.DataTo(&bc)
				require.NoError(t, err)

				require.NotZero(t, bc.Timestamp)
				require.True(t, joinBOriginTime.Equal(bc.OriginTimestamp.AsTime()))
				require.Equal(t, participantB.ID, bc.ParticipantId)
				return nil
			},
		).
		Run(ctx)
	require.NoError(t, err)
}

func TestHandlerHandleParticipantJoinNotCreatedSession(t *testing.T) {
	clientA, _, close := NewTestingEnv(t, newTestHandler())
	defer close()

	err := scenario.NewScenario(clientA).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 1,
				SessionId: "helloxsession",
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
			scenario.FilterByRequestID(1),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ErrorResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_NOT_FOUND, res.Code)
				return err
			},
		).
		Run(context.Background())
	require.NoError(t, err)
}

func TestHandlerHandleMultipleSameParticipantJoins(t *testing.T) {
	clientA, clientB, close := NewTestingEnv(t, newTestHandler())
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var participantA models.Participant
	var participantB models.Participant
	var sessionID string

	err := scenario.NewScenario(clientA).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 1,
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			scenario.FilterByRequestID(1),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ParticipantJoinResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				participantA.ID = res.ParticipantId
				sessionID = res.SessionId
				return err
			}).
		Run(ctx)
	require.NoError(t, err)

	err = scenario.NewScenario(clientB).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 1,
				SessionId: sessionID,
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			scenario.FilterByRequestID(1),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ParticipantJoinResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				require.NotZero(t, res.ParticipantId)

				require.Equal(t, sessionID, res.SessionId)
				participantB.ID = res.ParticipantId
				return err
			}).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 2,
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			scenario.FilterByRequestID(2),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ParticipantJoinResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				require.NotEqual(t, sessionID, res.SessionId)
				require.NotEqual(t, participantB.ID, res.ParticipantId)
				return err
			}).
		Run(ctx)
	require.NoError(t, err)

	err = scenario.NewScenario(clientA).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_LEAVE_BROADCAST),
			func(msg hwebsocket.Msg) error {

				var bc hagallpb.ParticipantLeaveBroadcast
				err := msg.DataTo(&bc)
				require.NoError(t, err)
				require.Equal(t, participantB.ID, bc.ParticipantId)
				return err
			}).
		Run(ctx)
	require.NoError(t, err)
}

func TestHandlerHandleMultipleJoinWithSameSession(t *testing.T) {
	clientA, clientB, close := NewTestingEnv(t, newTestHandler())
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	var sessionID string

	err := scenario.NewScenario(clientA).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 1,
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			scenario.FilterByRequestID(1),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ParticipantJoinResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				sessionID = res.SessionId
				return err
			}).
		Run(ctx)
	require.NoError(t, err)

	err = scenario.NewScenario(clientB).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 1,
				SessionId: sessionID,
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			scenario.FilterByRequestID(1),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ParticipantJoinResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				require.Equal(t, sessionID, res.SessionId)
				return err
			}).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 2,
				SessionId: sessionID,
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
			scenario.FilterByRequestID(2),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ErrorResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_SESSION_ALREADY_JOINED, res.Code)
				return err
			}).
		Run(ctx)
	require.NoError(t, err)

	err = scenario.NewScenario(clientA).
		Receive(scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_LEAVE_BROADCAST)).
		Run(ctx)
	require.Error(t, err)
}

func TestHandlerHandleParticipantDisconnect(t *testing.T) {
	clientA, clientB, close := NewTestingEnv(t, newTestHandler())
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var sessionID string

	err := scenario.NewScenario(clientA).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 1,
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ParticipantJoinResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				sessionID = res.SessionId
				return err
			},
		).
		Run(ctx)
	require.NoError(t, err)

	var participantBID uint32
	var entityID uint32

	err = scenario.NewScenario(clientB).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 2,
				SessionId: sessionID,
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ParticipantJoinResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				participantBID = res.ParticipantId
				return err
			},
		).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.EntityAddRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 3,
			}
		}).
		Receive(
			scenario.FilterByRequestID(3),
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_RESPONSE),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.EntityAddResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				entityID = res.EntityId
				return err
			},
		).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.EntityAddRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 4,
				Persist:   true,
			}
		}).
		Receive(
			scenario.FilterByRequestID(4),
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_RESPONSE),
		).
		Run(ctx)
	require.NoError(t, err)

	clientB.Close()

	err = scenario.NewScenario(clientA).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_DELETE_BROADCAST),
			func(msg hwebsocket.Msg) error {
				var bc hagallpb.EntityDeleteBroadcast
				err := msg.DataTo(&bc)
				require.NoError(t, err)

				require.NotZero(t, bc.Timestamp)
				require.NotZero(t, bc.OriginTimestamp)
				require.Equal(t, entityID, bc.EntityId)
				return err
			},
		).
		Receive(
			scenario.FilterByType(
				hagallpb.MsgType_MSG_TYPE_ENTITY_DELETE_BROADCAST,
				hagallpb.MsgType_MSG_TYPE_PARTICIPANT_LEAVE_BROADCAST,
			),
			func(msg hwebsocket.Msg) error {
				require.NotEqual(t, hagallpb.MsgType_MSG_TYPE_ENTITY_DELETE_BROADCAST, msg.Type)

				var bc hagallpb.ParticipantLeaveBroadcast
				err := msg.DataTo(&bc)
				require.NoError(t, err)

				require.NotZero(t, bc.Timestamp)
				require.NotZero(t, bc.OriginTimestamp)
				require.Equal(t, participantBID, bc.ParticipantId)
				return err
			},
		).
		Run(ctx)
	require.NoError(t, err)
}

func TestHandlerHandleEntityAdd(t *testing.T) {
	clientA, clientB, close := NewTestingEnv(t, newTestHandler())
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var sessionID string

	err := scenario.NewScenario(clientA).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 1,
			}
		}).
		Receive(scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE), func(msg hwebsocket.Msg) error {
			var res hagallpb.ParticipantJoinResponse
			err := msg.DataTo(&res)
			require.NoError(t, err)

			sessionID = res.SessionId
			return err
		}).
		Run(ctx)
	require.NoError(t, err)

	entity := models.Entity{
		Persist: true,
		Flag:    hagallpb.EntityFlag_ENTITY_FLAG_PARTICIPANT_ENTITY,
	}
	entity.SetPose(
		models.Pose{
			PX: 1,
			PY: 2,
			PZ: 3,
			RX: 4,
			RY: 5,
			RZ: 6,
			RW: 7,
		},
	)

	var entityAddBOriginTime time.Time

	err = scenario.NewScenario(clientB).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 2,
				SessionId: sessionID,
			}
		}).
		Receive(scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE), func(msg hwebsocket.Msg) error {
			var res hagallpb.ParticipantJoinResponse
			err := msg.DataTo(&res)
			require.NoError(t, err)

			entity.ParticipantID = res.ParticipantId
			return err
		}).
		Send(func() hwebsocket.ProtoMsg {
			entityAddBOriginTime = time.Now()

			return &hagallpb.EntityAddRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_REQUEST,
				Timestamp: timestamppb.New(entityAddBOriginTime),
				RequestId: 3,
				Pose:      entity.Pose().ToProtobuf(),
				Persist:   entity.Persist,
				Flag:      entity.Flag,
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_RESPONSE),
			scenario.FilterByRequestID(3),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.EntityAddResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				require.NotZero(t, res.Timestamp)
				require.NotZero(t, res.EntityId)

				entity.ID = res.EntityId
				return err
			},
		).
		Run(ctx)
	require.NoError(t, err)

	err = scenario.NewScenario(clientA).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_BROADCAST),
			func(msg hwebsocket.Msg) error {
				var bc hagallpb.EntityAddBroadcast
				err := msg.DataTo(&bc)
				require.NoError(t, err)

				require.NotZero(t, bc.Timestamp)
				require.True(t, entityAddBOriginTime.Equal(bc.OriginTimestamp.AsTime()))

				require.Equal(t, entity.ID, bc.Entity.Id)
				require.Equal(t, entity.ParticipantID, bc.Entity.ParticipantId)
				require.Equal(t, entity.Pose().PX, bc.Entity.Pose.Px)
				require.Equal(t, entity.Pose().PY, bc.Entity.Pose.Py)
				require.Equal(t, entity.Pose().PZ, bc.Entity.Pose.Pz)
				require.Equal(t, entity.Pose().RX, bc.Entity.Pose.Rx)
				require.Equal(t, entity.Pose().RY, bc.Entity.Pose.Ry)
				require.Equal(t, entity.Pose().RZ, bc.Entity.Pose.Rz)
				require.Equal(t, entity.Pose().RW, bc.Entity.Pose.Rw)
				require.Equal(t, entity.Flag, bc.Entity.Flag)
				return err
			},
		).
		Run(ctx)
	require.NoError(t, err)
}

func TestHandlerHandleEntityAddSessionNotJoined(t *testing.T) {
	clientA, _, close := NewTestingEnv(t, newTestHandler())
	defer close()

	err := scenario.NewScenario(clientA).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.EntityAddRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 1,
			}
		}).
		Receive().
		Run(context.Background())
	require.Error(t, err)
}

func TestHandlerHandleEntityDelete(t *testing.T) {
	clientA, clientB, close := NewTestingEnv(t, newTestHandler())
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var sessionID string

	err := scenario.NewScenario(clientA).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 1,
			}
		}).
		Receive(scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE), func(msg hwebsocket.Msg) error {
			var res hagallpb.ParticipantJoinResponse
			err := msg.DataTo(&res)
			require.NoError(t, err)

			sessionID = res.SessionId
			return err
		}).
		Run(ctx)
	require.NoError(t, err)

	var entity models.Entity
	var entityDeleteBOriginTime time.Time

	err = scenario.NewScenario(clientB).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 2,
				SessionId: sessionID,
			}
		}).
		Receive(scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE), func(msg hwebsocket.Msg) error {
			var res hagallpb.ParticipantJoinResponse
			err := msg.DataTo(&res)
			require.NoError(t, err)

			entity.ParticipantID = res.ParticipantId
			return err
		}).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.EntityAddRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 3,
				Pose:      entity.Pose().ToProtobuf(),
				Persist:   entity.Persist,
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_RESPONSE),
			scenario.FilterByRequestID(3),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.EntityAddResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				entity.ID = res.EntityId
				return err
			},
		).
		Send(func() hwebsocket.ProtoMsg {
			entityDeleteBOriginTime = time.Now()

			return &hagallpb.EntityDeleteRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_DELETE_REQUEST,
				Timestamp: timestamppb.New(entityDeleteBOriginTime),
				RequestId: 4,
				EntityId:  entity.ID,
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_DELETE_RESPONSE),
			scenario.FilterByRequestID(4),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.EntityDeleteResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				require.NotZero(t, res.Timestamp)
				return err
			},
		).
		Run(ctx)
	require.NoError(t, err)

	err = scenario.NewScenario(clientA).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_DELETE_BROADCAST),
			func(msg hwebsocket.Msg) error {
				var bc hagallpb.EntityDeleteBroadcast
				err := msg.DataTo(&bc)
				require.NoError(t, err)

				require.NotZero(t, bc.Timestamp)
				require.True(t, entityDeleteBOriginTime.Equal(bc.OriginTimestamp.AsTime()))
				require.Equal(t, entity.ID, bc.EntityId)
				return err
			},
		).
		Run(ctx)
	require.NoError(t, err)
}

func TestHandlerHandleEntityDeleteNotOwned(t *testing.T) {
	clientA, clientB, close := NewTestingEnv(t, newTestHandler())
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var sessionID string
	var entityID uint32

	err := scenario.NewScenario(clientA).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 1,
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ParticipantJoinResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				sessionID = res.SessionId
				return err
			},
		).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.EntityAddRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 2,
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_RESPONSE),
			scenario.FilterByRequestID(2),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.EntityAddResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				entityID = res.EntityId
				return err
			},
		).
		Run(ctx)
	require.NoError(t, err)

	err = scenario.NewScenario(clientB).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 3,
				SessionId: sessionID,
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			scenario.FilterByRequestID(3),
		).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.EntityDeleteRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_DELETE_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 4,
				EntityId:  entityID,
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
			scenario.FilterByRequestID(4),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ErrorResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				require.NotZero(t, res.Timestamp)
				require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_UNAUTHORIZED, res.Code)
				return err
			},
		).
		Run(ctx)
	require.NoError(t, err)
}

func TestHandlerHandleEntityDeleteNonexistent(t *testing.T) {
	clientA, _, close := NewTestingEnv(t, newTestHandler())
	defer close()

	err := scenario.NewScenario(clientA).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 1,
			}
		}).
		Receive(scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE)).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.EntityDeleteRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_DELETE_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 2,
				EntityId:  42,
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
			scenario.FilterByRequestID(2),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ErrorResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				require.NotZero(t, res.Timestamp)
				require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_NOT_FOUND, res.Code)
				return err
			},
		).
		Run(context.Background())
	require.NoError(t, err)
}

func TestHandlerHandleEntityDeleteSessionNotJoined(t *testing.T) {
	clientA, _, close := NewTestingEnv(t, newTestHandler())
	defer close()

	err := scenario.NewScenario(clientA).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.EntityDeleteRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_DELETE_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 1,
				EntityId:  1,
			}
		}).
		Receive().
		Run(context.Background())
	require.Error(t, err)
}

func TestHandlerHandlerEntityPoseUpdate(t *testing.T) {
	clientA, clientB, close := NewTestingEnv(t, newTestHandler())
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var sessionID string

	err := scenario.NewScenario(clientA).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 1,
			}
		}).
		Receive(scenario.FilterByType(
			hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ParticipantJoinResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				sessionID = res.SessionId
				return err
			},
		).
		Run(ctx)
	require.NoError(t, err)

	var entity models.Entity
	var updatePoseBTime time.Time

	err = scenario.NewScenario(clientB).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 2,
				SessionId: sessionID,
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ParticipantJoinResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				sessionID = res.SessionId
				entity.ParticipantID = res.ParticipantId
				return err
			},
		).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.EntityAddRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 3,
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_RESPONSE),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.EntityAddResponse
				err := msg.DataTo(&res)

				entity.ID = res.EntityId
				return err
			},
		).
		Send(func() hwebsocket.ProtoMsg {
			updatePoseBTime = time.Now()

			entity.SetPose(models.Pose{
				PX: 11,
				PY: 12,
				PZ: 13,
				RX: 14,
				RY: 15,
				RZ: 16,
				RW: 17,
			})

			return &hagallpb.EntityUpdatePose{
				Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_UPDATE_POSE,
				Timestamp: timestamppb.New(updatePoseBTime),
				EntityId:  entity.ID,
				Pose:      entity.Pose().ToProtobuf(),
			}
		}).
		Run(ctx)
	require.NoError(t, err)

	err = scenario.NewScenario(clientA).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_UPDATE_POSE_BROADCAST),
			func(msg hwebsocket.Msg) error {
				var bc hagallpb.EntityUpdatePoseBroadcast
				err := msg.DataTo(&bc)
				require.NoError(t, err)

				require.NotZero(t, bc.Timestamp)
				require.True(t, updatePoseBTime.Equal(bc.OriginTimestamp.AsTime()))
				require.Equal(t, entity.ID, bc.EntityId)
				require.Equal(t, entity.Pose().PX, bc.Pose.Px)
				require.Equal(t, entity.Pose().PY, bc.Pose.Py)
				require.Equal(t, entity.Pose().PZ, bc.Pose.Pz)
				require.Equal(t, entity.Pose().RX, bc.Pose.Rx)
				require.Equal(t, entity.Pose().RY, bc.Pose.Ry)
				require.Equal(t, entity.Pose().RZ, bc.Pose.Rz)
				require.Equal(t, entity.Pose().RW, bc.Pose.Rw)
				return err
			},
		).
		Run(ctx)
	require.NoError(t, err)
}

func TestHandlerHandleEntityUpdatePoseSessionNotJoined(t *testing.T) {
	clientA, _, close := NewTestingEnv(t, newTestHandler())
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := scenario.NewScenario(clientA).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.EntityUpdatePose{
				Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_UPDATE_POSE,
				Timestamp: timestamppb.Now(),
				EntityId:  1,
			}
		}).
		Receive().
		Run(ctx)
	require.NoError(t, err)
}

func TestHandlerHandleEntityUpdatePoseNotFound(t *testing.T) {
	clientA, clientB, close := NewTestingEnv(t, newTestHandler())
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var sessionID string

	err := scenario.NewScenario(clientA).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 1,
			}
		}).
		Receive(scenario.FilterByType(
			hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ParticipantJoinResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				sessionID = res.SessionId
				return err
			},
		).
		Run(ctx)
	require.NoError(t, err)

	err = scenario.NewScenario(clientB).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 2,
				SessionId: sessionID,
			}
		}).
		Receive(scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE)).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.EntityUpdatePose{
				Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_UPDATE_POSE,
				Timestamp: timestamppb.Now(),
				EntityId:  42,
			}
		}).
		Run(ctx)
	require.NoError(t, err)

	ctxTimeout, cancelTimeout := context.WithTimeout(ctx, time.Millisecond)
	defer cancelTimeout()

	err = scenario.NewScenario(clientA).
		Receive(scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_UPDATE_POSE_BROADCAST)).
		Run(ctxTimeout)
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestHandlerHandleEntityUpdateNotOwned(t *testing.T) {
	clientA, clientB, close := NewTestingEnv(t, newTestHandler())
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var sessionID string
	var entityID uint32

	err := scenario.NewScenario(clientA).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 1,
			}
		}).
		Receive(scenario.FilterByType(
			hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ParticipantJoinResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				sessionID = res.SessionId
				return err
			},
		).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.EntityAddRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 2,
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_RESPONSE),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.EntityAddResponse
				err := msg.DataTo(&res)

				entityID = res.EntityId
				return err
			},
		).
		Run(ctx)
	require.NoError(t, err)

	err = scenario.NewScenario(clientB).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 2,
				SessionId: sessionID,
			}
		}).
		Receive(scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE)).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.EntityUpdatePose{
				Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_UPDATE_POSE,
				Timestamp: timestamppb.Now(),
				EntityId:  entityID,
			}
		}).
		Run(ctx)
	require.NoError(t, err)

	ctxTimeout, cancelTimeout := context.WithTimeout(ctx, time.Millisecond)
	defer cancelTimeout()

	err = scenario.NewScenario(clientA).
		Receive(scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_UPDATE_POSE_BROADCAST)).
		Run(ctxTimeout)
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestHandleCustomMessage(t *testing.T) {
	t.Run("custom message is broadcasted to everyone", func(t *testing.T) {
		clientA, clientB, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		var sessionID string

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(scenario.FilterByType(
				hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ParticipantJoinResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					sessionID = res.SessionId
					return err
				},
			).
			Run(ctx)
		require.NoError(t, err)

		body := []byte("hello")
		var participantBID uint32
		var cutomMsgTime time.Time

		err = scenario.NewScenario(clientB).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 2,
					SessionId: sessionID,
				}
			}).
			Receive(
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ParticipantJoinResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					sessionID = res.SessionId
					participantBID = res.ParticipantId
					return err
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				cutomMsgTime = time.Now()

				return &hagallpb.CustomMessage{
					Type:      hagallpb.MsgType_MSG_TYPE_CUSTOM_MESSAGE,
					Timestamp: timestamppb.New(cutomMsgTime),
					Body:      body,
				}
			}).
			Run(ctx)
		require.NoError(t, err)

		err = scenario.NewScenario(clientA).
			Receive(
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_CUSTOM_MESSAGE_BROADCAST),
				func(msg hwebsocket.Msg) error {
					var bc hagallpb.CustomMessageBroadcast
					err := msg.DataTo(&bc)
					require.NoError(t, err)

					require.NotZero(t, bc.Timestamp)
					require.True(t, cutomMsgTime.Equal(bc.OriginTimestamp.AsTime()))
					require.Equal(t, participantBID, bc.ParticipantId)
					require.Equal(t, body, bc.Body)
					return err
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})

	t.Run("custom message is broadcasted to participant B", func(t *testing.T) {
		clientA, clientB, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		var sessionID string
		var participantAID uint32

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(scenario.FilterByType(
				hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ParticipantJoinResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					sessionID = res.SessionId
					participantAID = res.ParticipantId
					return err
				},
			).
			Run(ctx)
		require.NoError(t, err)

		body := []byte("hello")
		var participantBID uint32

		err = scenario.NewScenario(clientB).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 2,
					SessionId: sessionID,
				}
			}).
			Receive(
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ParticipantJoinResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					sessionID = res.SessionId
					participantBID = res.ParticipantId
					return err
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.CustomMessage{
					Type:           hagallpb.MsgType_MSG_TYPE_CUSTOM_MESSAGE,
					Timestamp:      timestamppb.Now(),
					ParticipantIds: []uint32{participantAID},
					Body:           body,
				}
			}).
			Run(ctx)
		require.NoError(t, err)

		err = scenario.NewScenario(clientA).
			Receive(
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_CUSTOM_MESSAGE_BROADCAST),
				func(msg hwebsocket.Msg) error {
					var bc hagallpb.CustomMessageBroadcast
					err := msg.DataTo(&bc)
					require.NoError(t, err)

					require.NotZero(t, bc.Timestamp)
					require.NotZero(t, bc.OriginTimestamp)
					require.Equal(t, body, bc.Body)
					require.Equal(t, participantBID, bc.ParticipantId)
					return err
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})

	t.Run("message larger than 10 kB are rejected with error", func(t *testing.T) {
		clientA, clientB, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond*100)
		defer cancel()

		var sessionID string
		var participantAID uint32

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(scenario.FilterByType(
				hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ParticipantJoinResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					sessionID = res.SessionId
					participantAID = res.ParticipantId
					return err
				},
			).
			Run(ctx)
		require.NoError(t, err)

		var body string
		for len(body) <= customMessageMaxSize {
			body += uuid.NewString()
		}

		err = scenario.NewScenario(clientB).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 2,
					SessionId: sessionID,
				}
			}).
			Receive(
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ParticipantJoinResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					sessionID = res.SessionId
					return err
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.CustomMessage{
					Type:           hagallpb.MsgType_MSG_TYPE_CUSTOM_MESSAGE,
					Timestamp:      timestamppb.Now(),
					ParticipantIds: []uint32{participantAID},
					Body:           []byte(body),
				}
			}).
			Receive(
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ErrorResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_TOO_LARGE, res.Code)

					return err
				},
			).
			Run(ctx)
		require.NoError(t, err)

		err = scenario.NewScenario(clientA).
			Receive(
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_CUSTOM_MESSAGE_BROADCAST),
			).
			Run(ctx)
		require.Error(t, err)
	})

	t.Run("sending a custom message while not in a session fails", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.CustomMessage{
					Type:      hagallpb.MsgType_MSG_TYPE_CUSTOM_MESSAGE,
					Timestamp: timestamppb.Now(),
					Body:      []byte("hello"),
				}
			}).
			Receive().
			Run(context.Background())
		require.Error(t, err)
	})
}

func TestHandlerDisconnectOnIdleTimeout(t *testing.T) {
	clientA, _, close := newTestingEnv(t, func() Handler {
		return &RealtimeHandler{
			ClientSyncClockInterval: time.Second,
			ClientIdleTimeout:       0,
			Sessions:                &models.SessionStore{},
		}
	})
	defer close()

	err := scenario.NewScenario(clientA).
		Receive(func(msg hwebsocket.Msg) error {
			return scenario.ErrScenarioMsgSkip
		}).
		Run(context.Background())
	require.Error(t, err)
}

func TestHandlerHandleEntityComponentTypeAdd(t *testing.T) {
	t.Run("request with missing component name returns bad request error", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeAddRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_ADD_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(
				scenario.FilterByRequestID(1),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ErrorResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.NotZero(t, res.Timestamp)
					require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_BAD_REQUEST, res.Code)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})

	t.Run("request without joining a session terminates the connection", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeAddRequest{
					Type:                    hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_ADD_REQUEST,
					Timestamp:               timestamppb.Now(),
					EntityComponentTypeName: "foo",
				}
			}).
			Receive().
			Run(context.Background())
		require.Error(t, err)
	})

	t.Run("entity component is succesfully registered", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(scenario.FilterByType(
				hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ParticipantJoinResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					return err
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeAddRequest{
					Type:                    hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_ADD_REQUEST,
					Timestamp:               timestamppb.Now(),
					RequestId:               2,
					EntityComponentTypeName: "foo",
				}
			}).
			Receive(
				scenario.FilterByRequestID(2),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_ADD_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityComponentTypeAddResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.NotZero(t, res.Timestamp)
					require.NotZero(t, res.EntityComponentTypeId)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})
}

func TestHandlerHandleEntityComponentGetName(t *testing.T) {
	t.Run("request with missing component id returns bad request error", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeGetNameRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_GET_NAME_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(
				scenario.FilterByRequestID(1),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ErrorResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.NotZero(t, res.Timestamp)
					require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_BAD_REQUEST, res.Code)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})

	t.Run("request without joining a session terminates the connection", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeGetNameRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_GET_NAME_REQUEST,
					Timestamp:             timestamppb.Now(),
					EntityComponentTypeId: 42,
				}
			}).
			Receive().
			Run(context.Background())
		require.Error(t, err)
	})

	t.Run("requesting non registered entity component name returns a not found error", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(scenario.FilterByType(
				hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ParticipantJoinResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					return err
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeGetNameRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_GET_NAME_REQUEST,
					Timestamp:             timestamppb.Now(),
					RequestId:             2,
					EntityComponentTypeId: 42,
				}
			}).
			Receive(
				scenario.FilterByRequestID(2),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ErrorResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.NotZero(t, res.Timestamp)
					require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_NOT_FOUND, res.Code)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})

	t.Run("entity component name is succesfully returned", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		entityEntityComponentTypeName := "foo"
		var entityEntityComponentTypeId uint32

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(scenario.FilterByType(
				hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ParticipantJoinResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					return err
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeAddRequest{
					Type:                    hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_ADD_REQUEST,
					Timestamp:               timestamppb.Now(),
					RequestId:               2,
					EntityComponentTypeName: entityEntityComponentTypeName,
				}
			}).
			Receive(
				scenario.FilterByRequestID(2),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_ADD_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityComponentTypeAddResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					entityEntityComponentTypeId = res.EntityComponentTypeId
					return nil
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeGetNameRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_GET_NAME_REQUEST,
					Timestamp:             timestamppb.Now(),
					RequestId:             3,
					EntityComponentTypeId: entityEntityComponentTypeId,
				}
			}).
			Receive(
				scenario.FilterByRequestID(3),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_GET_NAME_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityComponentTypeGetNameResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.NotZero(t, res.Timestamp)
					require.Equal(t, entityEntityComponentTypeName, res.EntityComponentTypeName)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})
}

func TestHandlerHandleEntityComponentGetID(t *testing.T) {
	t.Run("request with missing component name returns bad request error", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeGetIdRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_GET_ID_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(
				scenario.FilterByRequestID(1),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ErrorResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.NotZero(t, res.Timestamp)
					require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_BAD_REQUEST, res.Code)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})

	t.Run("request without joining a session terminates the connection", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeGetIdRequest{
					Type:                    hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_GET_ID_REQUEST,
					Timestamp:               timestamppb.Now(),
					EntityComponentTypeName: "foo",
				}
			}).
			Receive().
			Run(context.Background())
		require.Error(t, err)
	})

	t.Run("requesting non registered entity component id returns a not found error", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(scenario.FilterByType(
				hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ParticipantJoinResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					return err
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeGetIdRequest{
					Type:                    hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_GET_ID_REQUEST,
					Timestamp:               timestamppb.Now(),
					RequestId:               2,
					EntityComponentTypeName: "foo",
				}
			}).
			Receive(
				scenario.FilterByRequestID(2),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ErrorResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.NotZero(t, res.Timestamp)
					require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_NOT_FOUND, res.Code)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})

	t.Run("entity component id is succesfully returned", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		entityEntityComponentTypeName := "foo"
		var entityEntityComponentTypeId uint32

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(scenario.FilterByType(
				hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ParticipantJoinResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					return err
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeAddRequest{
					Type:                    hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_ADD_REQUEST,
					Timestamp:               timestamppb.Now(),
					RequestId:               2,
					EntityComponentTypeName: entityEntityComponentTypeName,
				}
			}).
			Receive(
				scenario.FilterByRequestID(2),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_ADD_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityComponentTypeAddResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					entityEntityComponentTypeId = res.EntityComponentTypeId
					return nil
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeGetIdRequest{
					Type:                    hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_GET_ID_REQUEST,
					Timestamp:               timestamppb.Now(),
					RequestId:               3,
					EntityComponentTypeName: entityEntityComponentTypeName,
				}
			}).
			Receive(
				scenario.FilterByRequestID(3),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_GET_ID_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityComponentTypeGetIdResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.NotZero(t, res.Timestamp)
					require.Equal(t, entityEntityComponentTypeId, res.EntityComponentTypeId)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})
}

func TestHandlerHandleEntityComponentAdd(t *testing.T) {
	t.Run("request with missing component id or entity id returns bad request error", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentAddRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_ADD_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(
				scenario.FilterByRequestID(1),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ErrorResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.NotZero(t, res.Timestamp)
					require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_BAD_REQUEST, res.Code)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})

	t.Run("request without joining a session terminates the connection", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentAddRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_ADD_REQUEST,
					Timestamp:             timestamppb.Now(),
					EntityComponentTypeId: 42,
					EntityId:              21,
				}
			}).
			Receive().
			Run(context.Background())
		require.Error(t, err)
	})

	t.Run("adding an entity component whitout an existing entity returns a not found error", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		var entityEntityComponentTypeId uint32

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(scenario.FilterByType(
				hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ParticipantJoinResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					return err
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeAddRequest{
					Type:                    hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_ADD_REQUEST,
					Timestamp:               timestamppb.Now(),
					RequestId:               2,
					EntityComponentTypeName: "foo",
				}
			}).
			Receive(
				scenario.FilterByRequestID(2),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_ADD_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityComponentTypeAddResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					entityEntityComponentTypeId = res.EntityComponentTypeId
					return nil
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentAddRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_ADD_REQUEST,
					Timestamp:             timestamppb.Now(),
					RequestId:             3,
					EntityComponentTypeId: entityEntityComponentTypeId,
					EntityId:              21,
				}
			}).
			Receive(
				scenario.FilterByRequestID(3),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ErrorResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.NotZero(t, res.Timestamp)
					require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_NOT_FOUND, res.Code)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})

	t.Run("adding an non registered entity component returns a not found error", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		var entityID uint32

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(
				scenario.FilterByRequestID(1),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityAddRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 2,
				}
			}).
			Receive(
				scenario.FilterByRequestID(2),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityAddResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					entityID = res.EntityId
					return err
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentAddRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_ADD_REQUEST,
					Timestamp:             timestamppb.Now(),
					RequestId:             3,
					EntityId:              entityID,
					EntityComponentTypeId: 42,
				}
			}).
			Receive(
				scenario.FilterByRequestID(3),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ErrorResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.NotZero(t, res.Timestamp)
					require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_NOT_FOUND, res.Code)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err, entityID)
	})

	t.Run("entity component is successfully added", func(t *testing.T) {
		clientA, clientB, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		var sessionID string
		var entityID uint32
		var entityEntityComponentTypeId uint32
		entityComponentData := []byte("ted's pose")

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(
				scenario.FilterByRequestID(1),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ParticipantJoinResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					sessionID = res.SessionId
					return nil
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityAddRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 2,
				}
			}).
			Receive(
				scenario.FilterByRequestID(2),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityAddResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					entityID = res.EntityId
					return err
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeAddRequest{
					Type:                    hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_ADD_REQUEST,
					Timestamp:               timestamppb.Now(),
					RequestId:               3,
					EntityComponentTypeName: "foo",
				}
			}).
			Receive(
				scenario.FilterByRequestID(3),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_ADD_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityComponentTypeAddResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					entityEntityComponentTypeId = res.EntityComponentTypeId
					return nil
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentAddRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_ADD_REQUEST,
					Timestamp:             timestamppb.Now(),
					RequestId:             4,
					EntityId:              entityID,
					EntityComponentTypeId: entityEntityComponentTypeId,
					Data:                  entityComponentData,
				}
			}).
			Receive(
				scenario.FilterByRequestID(4),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_ADD_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityComponentAddResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.NotZero(t, res.Timestamp)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err, entityID)

		err = scenario.NewScenario(clientB).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
					SessionId: sessionID,
				}
			}).
			Receive(
				scenario.FilterByRequestID(1),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			).
			Receive(
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_SESSION_STATE),
				func(msg hwebsocket.Msg) error {
					var state hagallpb.SessionState
					err := msg.DataTo(&state)
					require.NoError(t, err)

					require.Len(t, state.EntityComponents, 1)
					entityComponent := state.EntityComponents[0]
					require.Equal(t, entityEntityComponentTypeId, entityComponent.EntityComponentTypeId)
					require.Equal(t, entityID, entityComponent.EntityId)
					require.Equal(t, entityComponentData, entityComponent.Data)
					return err
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})

	t.Run("adding an already added entity component return a conflict error", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		var entityID uint32
		var entityEntityComponentTypeId uint32

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(
				scenario.FilterByRequestID(1),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityAddRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 2,
				}
			}).
			Receive(
				scenario.FilterByRequestID(2),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityAddResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					entityID = res.EntityId
					return err
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeAddRequest{
					Type:                    hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_ADD_REQUEST,
					Timestamp:               timestamppb.Now(),
					RequestId:               3,
					EntityComponentTypeName: "foo",
				}
			}).
			Receive(
				scenario.FilterByRequestID(3),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_ADD_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityComponentTypeAddResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					entityEntityComponentTypeId = res.EntityComponentTypeId
					return nil
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentAddRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_ADD_REQUEST,
					Timestamp:             timestamppb.Now(),
					RequestId:             4,
					EntityId:              entityID,
					EntityComponentTypeId: entityEntityComponentTypeId,
				}
			}).
			Receive(
				scenario.FilterByRequestID(4),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_ADD_RESPONSE),
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentAddRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_ADD_REQUEST,
					Timestamp:             timestamppb.Now(),
					RequestId:             5,
					EntityId:              entityID,
					EntityComponentTypeId: entityEntityComponentTypeId,
				}
			}).
			Receive(
				scenario.FilterByRequestID(5),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ErrorResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.NotZero(t, res.Timestamp)
					require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_CONFLICT, res.Code)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err, entityID)
	})
}

func TestHandlerHandleEntityComponentDelete(t *testing.T) {
	t.Run("request with missing component id or entity id returns bad request error", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentDeleteRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_DELETE_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(
				scenario.FilterByRequestID(1),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ErrorResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.NotZero(t, res.Timestamp)
					require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_BAD_REQUEST, res.Code)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})

	t.Run("request without joining a session terminates the connection", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentDeleteRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_DELETE_REQUEST,
					Timestamp:             timestamppb.Now(),
					EntityComponentTypeId: 42,
					EntityId:              21,
				}
			}).
			Receive().
			Run(context.Background())
		require.Error(t, err)
	})

	t.Run("deleting entity component from non existing entity returns a not found error", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(
				scenario.FilterByRequestID(1),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentDeleteRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_DELETE_REQUEST,
					Timestamp:             timestamppb.Now(),
					RequestId:             2,
					EntityComponentTypeId: 42,
					EntityId:              21,
				}
			}).
			Receive(
				scenario.FilterByRequestID(2),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ErrorResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.NotZero(t, res.Timestamp)
					require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_NOT_FOUND, res.Code)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})

	t.Run("deleting non added entity component return a not found error", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		var entityID uint32

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(
				scenario.FilterByRequestID(1),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityAddRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 2,
				}
			}).
			Receive(
				scenario.FilterByRequestID(2),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityAddResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					entityID = res.EntityId
					return nil
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentDeleteRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_DELETE_REQUEST,
					Timestamp:             timestamppb.Now(),
					RequestId:             3,
					EntityComponentTypeId: 42,
					EntityId:              entityID,
				}
			}).
			Receive(
				scenario.FilterByRequestID(3),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ErrorResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.NotZero(t, res.Timestamp)
					require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_NOT_FOUND, res.Code)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})

	t.Run("entity component is successfully deleted", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		var entityID uint32
		var entityEntityComponentTypeId uint32

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(
				scenario.FilterByRequestID(1),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityAddRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 2,
				}
			}).
			Receive(
				scenario.FilterByRequestID(2),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityAddResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					entityID = res.EntityId
					return nil
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeAddRequest{
					Type:                    hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_ADD_REQUEST,
					Timestamp:               timestamppb.Now(),
					RequestId:               3,
					EntityComponentTypeName: "foo",
				}
			}).
			Receive(
				scenario.FilterByRequestID(3),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_ADD_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityComponentTypeAddResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					entityEntityComponentTypeId = res.EntityComponentTypeId
					return nil
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentAddRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_ADD_REQUEST,
					Timestamp:             timestamppb.Now(),
					RequestId:             4,
					EntityComponentTypeId: entityEntityComponentTypeId,
					EntityId:              entityID,
				}
			}).
			Receive(
				scenario.FilterByRequestID(4),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_ADD_RESPONSE),
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentDeleteRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_DELETE_REQUEST,
					Timestamp:             timestamppb.Now(),
					RequestId:             5,
					EntityComponentTypeId: entityEntityComponentTypeId,
					EntityId:              entityID,
				}
			}).
			Receive(
				scenario.FilterByRequestID(5),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_DELETE_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityComponentDeleteResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.NotZero(t, res.Timestamp)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})
}

func TestHandlerHandleEntityComponentUpdate(t *testing.T) {
	t.Run("request with missing component id or entity id silently fails", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentUpdate{
					Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_UPDATE,
					Timestamp: timestamppb.Now(),
				}
			}).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentListRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_LIST_REQUEST,
					Timestamp:             timestamppb.Now(),
					RequestId:             2,
					EntityComponentTypeId: 42,
				}
			}).
			Receive().
			Run(ctx)
		require.Error(t, err)
	})

	t.Run("request without joining a session terminates the connection", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentUpdate{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_UPDATE,
					Timestamp:             timestamppb.Now(),
					EntityComponentTypeId: 42,
					EntityId:              21,
				}
			}).
			Receive().
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentListRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_LIST_REQUEST,
					Timestamp:             timestamppb.Now(),
					RequestId:             2,
					EntityComponentTypeId: 42,
				}
			}).
			Receive(
				scenario.FilterByRequestID(2),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_LIST_RESPONSE),
			).
			Run(context.Background())
		require.Error(t, err)
	})

	t.Run("updating entity component from non existing entity silently fails", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(
				scenario.FilterByRequestID(1),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentUpdate{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_UPDATE,
					Timestamp:             timestamppb.Now(),
					EntityComponentTypeId: 42,
					EntityId:              21,
					Data:                  []byte("foo"),
				}
			}).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentListRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_LIST_REQUEST,
					Timestamp:             timestamppb.Now(),
					RequestId:             3,
					EntityComponentTypeId: 42,
				}
			}).
			Receive(
				scenario.FilterByRequestID(3),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_LIST_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityComponentListResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.Empty(t, res.EntityComponents)
					return err
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})

	t.Run("entity component is successfully updated", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		var entityID uint32
		var entityEntityComponentTypeId uint32

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(
				scenario.FilterByRequestID(1),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityAddRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 2,
				}
			}).
			Receive(
				scenario.FilterByRequestID(2),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityAddResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					entityID = res.EntityId
					return nil
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeAddRequest{
					Type:                    hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_ADD_REQUEST,
					Timestamp:               timestamppb.Now(),
					RequestId:               3,
					EntityComponentTypeName: "foo",
				}
			}).
			Receive(
				scenario.FilterByRequestID(3),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_ADD_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityComponentTypeAddResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					entityEntityComponentTypeId = res.EntityComponentTypeId
					return nil
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentAddRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_ADD_REQUEST,
					Timestamp:             timestamppb.Now(),
					RequestId:             4,
					EntityComponentTypeId: entityEntityComponentTypeId,
					EntityId:              entityID,
					Data:                  []byte("hi"),
				}
			}).
			Receive(
				scenario.FilterByRequestID(4),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_ADD_RESPONSE),
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentUpdate{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_UPDATE,
					Timestamp:             timestamppb.Now(),
					EntityComponentTypeId: entityEntityComponentTypeId,
					EntityId:              entityEntityComponentTypeId,
					Data:                  []byte("bye"),
				}
			}).
			Receive().
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentListRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_LIST_REQUEST,
					Timestamp:             timestamppb.Now(),
					RequestId:             6,
					EntityComponentTypeId: entityEntityComponentTypeId,
				}
			}).
			Receive(
				scenario.FilterByRequestID(6),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_LIST_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityComponentListResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.NotZero(t, res.Timestamp)
					require.Len(t, res.EntityComponents, 1)

					entityComponent := res.EntityComponents[0]
					require.Equal(t, entityID, entityComponent.EntityId)
					require.Equal(t, entityEntityComponentTypeId, entityComponent.EntityComponentTypeId)
					require.Equal(t, []byte("bye"), entityComponent.Data)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})
}

func TestHandlerHandleEntityComponentList(t *testing.T) {
	t.Run("request with missing component id is ignored", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentListRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_LIST_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(
				scenario.FilterByRequestID(1),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ErrorResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.NotZero(t, res.Timestamp)
					require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_BAD_REQUEST, res.Code)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})

	t.Run("request without joining a session terminates the connection", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentListRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_LIST_REQUEST,
					Timestamp:             timestamppb.Now(),
					RequestId:             1,
					EntityComponentTypeId: 42,
				}
			}).
			Receive().
			Run(context.Background())
		require.Error(t, err)
	})

	t.Run("listing empty entity components returns successfully", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(
				scenario.FilterByRequestID(1),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentListRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_LIST_REQUEST,
					Timestamp:             timestamppb.Now(),
					RequestId:             2,
					EntityComponentTypeId: 42,
				}
			}).
			Receive(
				scenario.FilterByRequestID(2),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_LIST_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityComponentListResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.NotZero(t, res.Timestamp)
					require.Empty(t, res.EntityComponents)
					require.Nil(t, res.EntityComponents)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})

	t.Run("listing entity components returns successfully", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		var entityID uint32
		var entityEntityComponentTypeId uint32
		entityComponentData := []byte("hi")

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(
				scenario.FilterByRequestID(1),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityAddRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 2,
				}
			}).
			Receive(
				scenario.FilterByRequestID(2),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityAddResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					entityID = res.EntityId
					return nil
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeAddRequest{
					Type:                    hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_ADD_REQUEST,
					Timestamp:               timestamppb.Now(),
					RequestId:               3,
					EntityComponentTypeName: "foo",
				}
			}).
			Receive(
				scenario.FilterByRequestID(3),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_ADD_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityComponentTypeAddResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					entityEntityComponentTypeId = res.EntityComponentTypeId
					return nil
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentAddRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_ADD_REQUEST,
					Timestamp:             timestamppb.Now(),
					RequestId:             4,
					EntityComponentTypeId: entityEntityComponentTypeId,
					EntityId:              entityID,
					Data:                  entityComponentData,
				}
			}).
			Receive(
				scenario.FilterByRequestID(4),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_ADD_RESPONSE),
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentListRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_LIST_REQUEST,
					Timestamp:             timestamppb.Now(),
					RequestId:             5,
					EntityComponentTypeId: entityEntityComponentTypeId,
				}
			}).
			Receive(
				scenario.FilterByRequestID(5),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_LIST_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityComponentListResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.NotZero(t, res.Timestamp)
					require.Len(t, res.EntityComponents, 1)

					entityComponent := res.EntityComponents[0]
					require.Equal(t, entityID, entityComponent.EntityId)
					require.Equal(t, entityEntityComponentTypeId, entityComponent.EntityComponentTypeId)
					require.Equal(t, entityComponentData, entityComponent.Data)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})
}

func TestHandlerHandleEntityComponentSubscribe(t *testing.T) {
	t.Run("request with missing component id is ignored", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeSubscribeRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_SUBSCRIBE_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(
				scenario.FilterByRequestID(1),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ErrorResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.NotZero(t, res.Timestamp)
					require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_BAD_REQUEST, res.Code)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})

	t.Run("request without joining a session terminates the connection", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeSubscribeRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_SUBSCRIBE_REQUEST,
					Timestamp:             timestamppb.Now(),
					RequestId:             1,
					EntityComponentTypeId: 42,
				}
			}).
			Receive().
			Run(context.Background())
		require.Error(t, err)
	})

	t.Run("subscribing to a non registered entity component returns a not found error", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(
				scenario.FilterByRequestID(1),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeSubscribeRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_SUBSCRIBE_REQUEST,
					Timestamp:             timestamppb.Now(),
					RequestId:             2,
					EntityComponentTypeId: 42,
				}
			}).
			Receive(
				scenario.FilterByRequestID(2),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ErrorResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.NotZero(t, res.Timestamp)
					require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_NOT_FOUND, res.Code)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})

	t.Run("entity component subscription successfully done", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		var entityEntityComponentTypeId uint32

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(
				scenario.FilterByRequestID(1),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeAddRequest{
					Type:                    hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_ADD_REQUEST,
					Timestamp:               timestamppb.Now(),
					RequestId:               2,
					EntityComponentTypeName: "foo",
				}
			}).
			Receive(
				scenario.FilterByRequestID(2),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_ADD_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityComponentTypeAddResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					entityEntityComponentTypeId = res.EntityComponentTypeId
					return nil
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeSubscribeRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_SUBSCRIBE_REQUEST,
					Timestamp:             timestamppb.Now(),
					RequestId:             3,
					EntityComponentTypeId: entityEntityComponentTypeId,
				}
			}).
			Receive(
				scenario.FilterByRequestID(3),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_SUBSCRIBE_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityComponentTypeSubscribeResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.NotZero(t, res.Timestamp)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})
}

func TestHandlerHandleEntityComponentUnsubscribe(t *testing.T) {
	t.Run("request with missing component id is ignored", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeUnsubscribeRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_UNSUBSCRIBE_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(
				scenario.FilterByRequestID(1),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ErrorResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.NotZero(t, res.Timestamp)
					require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_BAD_REQUEST, res.Code)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})

	t.Run("request without joining a session terminates the connection", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeUnsubscribeRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_UNSUBSCRIBE_REQUEST,
					Timestamp:             timestamppb.Now(),
					RequestId:             1,
					EntityComponentTypeId: 42,
				}
			}).
			Receive().
			Run(context.Background())
		require.Error(t, err)
	})

	t.Run("entity component unsubscriptionv is successfully done", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		var entityEntityComponentTypeId uint32

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(
				scenario.FilterByRequestID(1),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeAddRequest{
					Type:                    hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_ADD_REQUEST,
					Timestamp:               timestamppb.Now(),
					RequestId:               2,
					EntityComponentTypeName: "foo",
				}
			}).
			Receive(
				scenario.FilterByRequestID(2),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_ADD_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityComponentTypeAddResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					entityEntityComponentTypeId = res.EntityComponentTypeId
					return nil
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeSubscribeRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_SUBSCRIBE_REQUEST,
					Timestamp:             timestamppb.Now(),
					RequestId:             3,
					EntityComponentTypeId: entityEntityComponentTypeId,
				}
			}).
			Receive(
				scenario.FilterByRequestID(3),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_SUBSCRIBE_RESPONSE),
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeUnsubscribeRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_UNSUBSCRIBE_REQUEST,
					Timestamp:             timestamppb.Now(),
					RequestId:             4,
					EntityComponentTypeId: entityEntityComponentTypeId,
				}
			}).
			Receive(
				scenario.FilterByRequestID(4),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_UNSUBSCRIBE_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityComponentTypeUnsubscribeResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.NotZero(t, res.Timestamp)
					return err
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})
}

func TestEntityComponentNotifications(t *testing.T) {
	t.Run("subscribed client is notified when entity component is added, updated and deleted", func(t *testing.T) {
		clientA, clientB, close := NewTestingEnv(t, newTestHandler())
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		var sessionID string
		var entityEntityComponentTypeId uint32
		var entityID uint32

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
				}
			}).
			Receive(
				scenario.FilterByRequestID(1),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ParticipantJoinResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					sessionID = res.SessionId
					return err
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeAddRequest{
					Type:                    hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_ADD_REQUEST,
					Timestamp:               timestamppb.Now(),
					RequestId:               2,
					EntityComponentTypeName: "foo",
				}
			}).
			Receive(
				scenario.FilterByRequestID(2),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_ADD_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityComponentTypeAddResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					entityEntityComponentTypeId = res.EntityComponentTypeId
					return err
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentTypeSubscribeRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_SUBSCRIBE_REQUEST,
					Timestamp:             timestamppb.Now(),
					RequestId:             3,
					EntityComponentTypeId: entityEntityComponentTypeId,
				}
			}).
			Receive(
				scenario.FilterByRequestID(3),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_SUBSCRIBE_RESPONSE),
			).
			Run(ctx)
		require.NoError(t, err)

		err = scenario.NewScenario(clientB).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
					SessionId: sessionID,
				}
			}).
			Receive(
				scenario.FilterByRequestID(1),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityAddRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 2,
				}
			}).
			Receive(
				scenario.FilterByRequestID(2),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityAddResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					entityID = res.EntityId
					return nil
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentAddRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_ADD_REQUEST,
					Timestamp:             timestamppb.Now(),
					RequestId:             4,
					EntityComponentTypeId: entityEntityComponentTypeId,
					EntityId:              entityID,
					Data:                  []byte("hi"),
				}
			}).
			Receive(
				scenario.FilterByRequestID(4),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_ADD_RESPONSE),
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentUpdate{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_UPDATE,
					Timestamp:             timestamppb.Now(),
					EntityComponentTypeId: entityEntityComponentTypeId,
					EntityId:              entityID,
					Data:                  []byte("bye"),
				}
			}).
			Receive().
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityComponentDeleteRequest{
					Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_DELETE_REQUEST,
					Timestamp:             timestamppb.Now(),
					RequestId:             6,
					EntityComponentTypeId: entityEntityComponentTypeId,
					EntityId:              entityID,
				}
			}).
			Receive(
				scenario.FilterByRequestID(6),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_DELETE_RESPONSE),
			).
			Run(ctx)
		require.NoError(t, err)

		err = scenario.NewScenario(clientA).
			Receive(
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_ADD_BROADCAST),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityComponentAddBroadcast
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.NotZero(t, res.Timestamp)
					require.NotZero(t, res.OriginTimestamp)
					require.NotEqual(t, res.Timestamp, res.OriginTimestamp)
					require.Equal(t, entityEntityComponentTypeId, res.EntityComponent.EntityComponentTypeId)
					require.Equal(t, entityID, res.EntityComponent.EntityId)
					require.Equal(t, []byte("hi"), res.EntityComponent.Data)
					return err
				},
			).
			Receive(
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_UPDATE_BROADCAST),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityComponentUpdateBroadcast
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.NotZero(t, res.Timestamp)
					require.NotZero(t, res.OriginTimestamp)
					require.NotEqual(t, res.Timestamp, res.OriginTimestamp)
					require.Equal(t, entityEntityComponentTypeId, res.EntityComponent.EntityComponentTypeId)
					require.Equal(t, entityID, res.EntityComponent.EntityId)
					require.Equal(t, []byte("bye"), res.EntityComponent.Data)
					return err
				},
			).
			Receive(
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_DELETE_BROADCAST),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityComponentDeleteBroadcast
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.NotZero(t, res.Timestamp)
					require.NotZero(t, res.OriginTimestamp)
					require.NotEqual(t, res.Timestamp, res.OriginTimestamp)
					require.Equal(t, entityEntityComponentTypeId, res.EntityComponent.EntityComponentTypeId)
					require.Equal(t, entityID, res.EntityComponent.EntityId)
					require.Nil(t, res.EntityComponent.Data)
					return err
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})
}

func TestHandlerFeatureFlagFilter(t *testing.T) {
	newHandler := func() Handler {
		sessionStore := &models.SessionStore{
			DiscoveryService: &testClient{},
		}

		filters := []string{string(featureflag.FlagDisableSessionState)}
		var h Handler = &RealtimeHandler{
			ClientSyncClockInterval: time.Millisecond * 250,
			ClientIdleTimeout:       time.Minute,
			FrameDuration:           time.Millisecond * 50,
			Sessions:                sessionStore,
			FeatureFlags:            featureflag.New(filters),
		}

		h = HandlerWithLogs(h, time.Millisecond*100)
		h = HandlerWithMetrics(h, "https://auki-test.com")
		return h
	}
	clientA, _, close := NewTestingEnv(t, newHandler)
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := scenario.NewScenario(clientA).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 1,
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			scenario.FilterByRequestID(1),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ParticipantJoinResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				require.NotZero(t, res.Timestamp)
				require.NotEmpty(t, res.SessionId)
				require.NotZero(t, res.ParticipantId)

				return err
			}).
		Receive(scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_SESSION_STATE)).
		Run(ctx)
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
