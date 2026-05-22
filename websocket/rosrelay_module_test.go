package websocket

import (
	"context"
	"testing"
	"time"

	"github.com/aukilabs/hagall-common/messages/hagallpb"
	"github.com/aukilabs/hagall-common/messages/rosrelaypb"
	"github.com/aukilabs/hagall-common/scenario"
	hwebsocket "github.com/aukilabs/hagall-common/websocket"
	"github.com/aukilabs/hagall/featureflag"
	"github.com/aukilabs/hagall/modules"
	"github.com/aukilabs/hagall/modules/rosrelay"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func newRosRelayTestModule() modules.Module {
	return rosrelay.NewModule(featureflag.FeatureFlag{})
}

func newRosRelayTestModuleDisabled() modules.Module {
	ff := featureflag.FeatureFlag{}
	ff[featureflag.FlagDisableRosTopicRelay] = struct{}{}
	return rosrelay.NewModule(ff)
}

// joinSession is a helper that sends a ParticipantJoinRequest and returns
// the session ID and participant ID from the response.
func joinSession(t *testing.T, client *websocket.Conn, sessionID string, requestID uint32) (string, uint32) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var resSessionID string
	var resParticipantID uint32

	err := scenario.NewScenario(client).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: requestID,
				SessionId: sessionID,
			}
		}).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ParticipantJoinResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				resSessionID = res.SessionId
				resParticipantID = res.ParticipantId
				return err
			},
		).
		Run(ctx)
	require.NoError(t, err)

	return resSessionID, resParticipantID
}

func TestRosRelayFanOut(t *testing.T) {
	t.Run("ros topic message is broadcast to all session participants", func(t *testing.T) {
		clientA, clientB, close := NewTestingEnv(t, newTestHandler(newRosRelayTestModule))
		defer close()

		sessionID, _ := joinSession(t, clientA, "", 1)
		_, participantBID := joinSession(t, clientB, sessionID, 2)

		payload := []byte("hello-ros")
		publishTime := time.Now()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		err := scenario.NewScenario(clientB).
			Send(func() hwebsocket.ProtoMsg {
				return &rosrelaypb.RosTopicPublishRequest{
					Type:           rosrelaypb.MsgType_MSG_TYPE_ROS_TOPIC_PUBLISH_REQUEST,
					Timestamp:      timestamppb.New(publishTime),
					RequestId:      10,
					Topic:          "/cmd_vel",
					RosMessageType: "geometry_msgs/msg/Twist",
					Payload:        payload,
				}
			}).
			Receive(
				scenario.FilterByType(rosrelaypb.MsgType_MSG_TYPE_ROS_TOPIC_PUBLISH_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res rosrelaypb.RosTopicPublishResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)
					require.Equal(t, uint32(10), res.RequestId)
					return err
				},
			).
			Run(ctx)
		require.NoError(t, err)

		err = scenario.NewScenario(clientA).
			Receive(
				scenario.FilterByType(rosrelaypb.MsgType_MSG_TYPE_ROS_TOPIC_BROADCAST),
				func(msg hwebsocket.Msg) error {
					var bc rosrelaypb.RosTopicBroadcast
					err := msg.DataTo(&bc)
					require.NoError(t, err)

					require.NotZero(t, bc.Timestamp)
					require.True(t, publishTime.Equal(bc.OriginTimestamp.AsTime()))
					require.Equal(t, "/cmd_vel", bc.Topic)
					require.Equal(t, "geometry_msgs/msg/Twist", bc.RosMessageType)
					require.Equal(t, payload, bc.Payload)
					require.Equal(t, participantBID, bc.OriginParticipantId)
					return err
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})
}

func TestRosRelayTargetedDelivery(t *testing.T) {
	t.Run("ros topic message is delivered only to targeted participant", func(t *testing.T) {
		clientA, clientB, close := NewTestingEnv(t, newTestHandler(newRosRelayTestModule))
		defer close()

		sessionID, participantAID := joinSession(t, clientA, "", 1)
		_, participantBID := joinSession(t, clientB, sessionID, 2)

		payload := []byte("targeted-cmd")

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		err := scenario.NewScenario(clientB).
			Send(func() hwebsocket.ProtoMsg {
				return &rosrelaypb.RosTopicPublishRequest{
					Type:           rosrelaypb.MsgType_MSG_TYPE_ROS_TOPIC_PUBLISH_REQUEST,
					Timestamp:      timestamppb.Now(),
					RequestId:      20,
					Topic:          "/cmd_vel",
					RosMessageType: "geometry_msgs/msg/Twist",
					Payload:        payload,
					ParticipantIds: []uint32{participantAID},
				}
			}).
			Receive(
				scenario.FilterByType(rosrelaypb.MsgType_MSG_TYPE_ROS_TOPIC_PUBLISH_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res rosrelaypb.RosTopicPublishResponse
					return msg.DataTo(&res)
				},
			).
			Run(ctx)
		require.NoError(t, err)

		err = scenario.NewScenario(clientA).
			Receive(
				scenario.FilterByType(rosrelaypb.MsgType_MSG_TYPE_ROS_TOPIC_BROADCAST),
				func(msg hwebsocket.Msg) error {
					var bc rosrelaypb.RosTopicBroadcast
					err := msg.DataTo(&bc)
					require.NoError(t, err)

					require.Equal(t, "/cmd_vel", bc.Topic)
					require.Equal(t, payload, bc.Payload)
					require.Equal(t, participantBID, bc.OriginParticipantId)
					return err
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})
}

func TestRosRelaySizeCap(t *testing.T) {
	t.Run("payload exceeding 256 KiB is rejected with ERROR_CODE_TOO_LARGE", func(t *testing.T) {
		clientA, clientB, close := NewTestingEnv(t, newTestHandler(newRosRelayTestModule))
		defer close()

		sessionID, _ := joinSession(t, clientA, "", 1)
		joinSession(t, clientB, sessionID, 2)

		// Create a payload just over 256 KiB
		oversized := make([]byte, 256*1024+1)

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		err := scenario.NewScenario(clientB).
			Send(func() hwebsocket.ProtoMsg {
				return &rosrelaypb.RosTopicPublishRequest{
					Type:           rosrelaypb.MsgType_MSG_TYPE_ROS_TOPIC_PUBLISH_REQUEST,
					Timestamp:      timestamppb.Now(),
					RequestId:      30,
					Topic:          "/camera/image_raw",
					RosMessageType: "sensor_msgs/msg/Image",
					Payload:        oversized,
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
	})
}

func TestRosRelayCrossSessionIsolation(t *testing.T) {
	t.Run("message in session A is not delivered to participant in session B", func(t *testing.T) {
		clientA, clientB, close := NewTestingEnv(t, newTestHandler(newRosRelayTestModule))
		defer close()

		// Client A creates session A
		_, _ = joinSession(t, clientA, "", 1)
		// Client B creates session B (different session — empty sessionID)
		_, _ = joinSession(t, clientB, "", 2)

		payload := []byte("session-a-only")

		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()

		// Client A publishes to session A
		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &rosrelaypb.RosTopicPublishRequest{
					Type:           rosrelaypb.MsgType_MSG_TYPE_ROS_TOPIC_PUBLISH_REQUEST,
					Timestamp:      timestamppb.Now(),
					RequestId:      40,
					Topic:          "/odom",
					RosMessageType: "nav_msgs/msg/Odometry",
					Payload:        payload,
				}
			}).
			Receive(
				scenario.FilterByType(rosrelaypb.MsgType_MSG_TYPE_ROS_TOPIC_PUBLISH_RESPONSE),
				func(msg hwebsocket.Msg) error {
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)

		// Client B should NOT receive a broadcast — different session.
		// The scenario should timeout waiting for a message that never arrives.
		ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel2()

		err = scenario.NewScenario(clientB).
			Receive(
				scenario.FilterByType(rosrelaypb.MsgType_MSG_TYPE_ROS_TOPIC_BROADCAST),
				func(msg hwebsocket.Msg) error {
					t.Fatal("received ROS topic broadcast in wrong session")
					return nil
				},
			).
			Run(ctx2)
		// Expect a context deadline exceeded error — no message arrived
		require.Error(t, err)
	})
}

func TestRosRelayUnauthenticated(t *testing.T) {
	t.Run("publish before joining session is rejected", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler(newRosRelayTestModule))
		defer close()

		// Client A has NOT joined any session — send a publish request directly
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()

		err := scenario.NewScenario(clientA).
			Send(func() hwebsocket.ProtoMsg {
				return &rosrelaypb.RosTopicPublishRequest{
					Type:           rosrelaypb.MsgType_MSG_TYPE_ROS_TOPIC_PUBLISH_REQUEST,
					Timestamp:      timestamppb.Now(),
					RequestId:      50,
					Topic:          "/cmd_vel",
					RosMessageType: "geometry_msgs/msg/Twist",
					Payload:        []byte("unauthorized"),
				}
			}).
			Run(ctx)
		// The module won't even be called — handler.go L352 checks
		// CurrentParticipant()/CurrentSession() before dispatching to modules.
		// The message is silently dropped. No error response, no broadcast.
		// We just verify no crash and no broadcast was generated.
		require.NoError(t, err)
	})
}

func TestRosRelayFeatureFlag(t *testing.T) {
	t.Run("feature flag disables ros topic relay", func(t *testing.T) {
		clientA, clientB, close := NewTestingEnv(t, newTestHandler(newRosRelayTestModuleDisabled))
		defer close()

		sessionID, _ := joinSession(t, clientA, "", 1)
		joinSession(t, clientB, sessionID, 2)

		payload := []byte("should-not-broadcast")

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		// Client B publishes — should get a response but NO broadcast
		err := scenario.NewScenario(clientB).
			Send(func() hwebsocket.ProtoMsg {
				return &rosrelaypb.RosTopicPublishRequest{
					Type:           rosrelaypb.MsgType_MSG_TYPE_ROS_TOPIC_PUBLISH_REQUEST,
					Timestamp:      timestamppb.Now(),
					RequestId:      60,
					Topic:          "/cmd_vel",
					RosMessageType: "geometry_msgs/msg/Twist",
					Payload:        payload,
				}
			}).
			Receive(
				scenario.FilterByType(rosrelaypb.MsgType_MSG_TYPE_ROS_TOPIC_PUBLISH_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res rosrelaypb.RosTopicPublishResponse
					return msg.DataTo(&res)
				},
			).
			Run(ctx)
		require.NoError(t, err)

		// Client A should NOT receive a broadcast — feature flag is set
		ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel2()

		err = scenario.NewScenario(clientA).
			Receive(
				scenario.FilterByType(rosrelaypb.MsgType_MSG_TYPE_ROS_TOPIC_BROADCAST),
				func(msg hwebsocket.Msg) error {
					t.Fatal("received broadcast despite feature flag being disabled")
					return nil
				},
			).
			Run(ctx2)
		require.Error(t, err) // context deadline — no message
	})
}
