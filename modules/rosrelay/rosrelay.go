package rosrelay

import (
	"context"

	"github.com/aukilabs/go-tooling/pkg/errors"
	"github.com/aukilabs/hagall-common/messages/hagallpb"
	"github.com/aukilabs/hagall-common/messages/rosrelaypb"
	hwebsocket "github.com/aukilabs/hagall-common/websocket"
	"github.com/aukilabs/hagall/featureflag"
	"github.com/aukilabs/hagall/models"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// rosTopicMaxPayloadSize is the maximum allowed payload size for a ROS2 topic
// message. 256 KiB is chosen as a reasonable starting point — large enough for
// most control and sensor topics (Twist, LaserScan, CompressedImage thumbnails)
// while protecting the relay from unbounded memory use. Raw camera frames and
// point clouds should be streamed out-of-band.
const rosTopicMaxPayloadSize = 256 * 1024

type Module struct {
	currentSession     *models.Session
	currentParticipant *models.Participant
	featureFlags       featureflag.FeatureFlag
}

func NewModule(ff featureflag.FeatureFlag) *Module {
	return &Module{
		featureFlags: ff,
	}
}

func (m *Module) Name() string {
	return "rosrelay"
}

func (m *Module) Init(s *models.Session, p *models.Participant) {
	m.currentSession = s
	m.currentParticipant = p
}

func (m *Module) HandleMsg(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	switch rosrelaypb.MsgType(msg.Type.Number()) {
	case rosrelaypb.MsgType_MSG_TYPE_ROS_TOPIC_PUBLISH_REQUEST:
		return m.handleRosTopicPublish(ctx, respond, msg)
	default:
		return hwebsocket.ErrModuleMsgSkip
	}
}

func (m *Module) HandleDisconnect() {}

func (m *Module) handleRosTopicPublish(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	var req rosrelaypb.RosTopicPublishRequest
	if err := msg.DataTo(&req); err != nil {
		return err
	}

	participant := m.currentParticipant
	session := m.currentSession
	if participant == nil || session == nil {
		return errors.New("session not joined").
			WithType(hwebsocket.ErrTypeSessionNotJoined).
			WithTag("msg_type", msg.Type)
	}

	if len(req.Payload) > rosTopicMaxPayloadSize {
		respond.Send(&hagallpb.ErrorResponse{
			Type:      hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE,
			Timestamp: timestamppb.Now(),
			RequestId: req.RequestId,
			Code:      hagallpb.ErrorCode_ERROR_CODE_TOO_LARGE,
		})
		return nil
	}

	now := timestamppb.Now()

	respond.Send(&rosrelaypb.RosTopicPublishResponse{
		Type:      rosrelaypb.MsgType_MSG_TYPE_ROS_TOPIC_PUBLISH_RESPONSE,
		Timestamp: now,
		RequestId: req.RequestId,
	})

	m.featureFlags.IfNotSet(featureflag.FlagDisableRosTopicRelay, func() {
		broadcast := rosrelaypb.RosTopicBroadcast{
			Type:                rosrelaypb.MsgType_MSG_TYPE_ROS_TOPIC_BROADCAST,
			Timestamp:           now,
			OriginTimestamp:     req.Timestamp,
			Topic:               req.Topic,
			RosMessageType:      req.RosMessageType,
			Payload:             req.Payload,
			OriginParticipantId: participant.ID,
		}

		if len(req.ParticipantIds) != 0 {
			session.BroadcastTo(participant, &broadcast, req.ParticipantIds...)
			return
		}

		session.Broadcast(participant, &broadcast)
	})

	return nil
}
