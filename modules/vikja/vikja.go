package vikja

import (
	"context"

	"github.com/aukilabs/go-tooling/pkg/errors"
	"github.com/aukilabs/hagall-common/messages/hagallpb"
	"github.com/aukilabs/hagall-common/messages/vikjapb"
	hwebsocket "github.com/aukilabs/hagall-common/websocket"
	"github.com/aukilabs/hagall/models"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Module struct {
	currentSession     *models.Session
	currentParticipant *models.Participant
	state              *State
}

func (m *Module) Name() string {
	return "vikja"
}

func (m *Module) Init(s *models.Session, p *models.Participant) {
	m.currentSession = s
	m.currentParticipant = p

	state, ok := s.ModuleState(m.Name())
	if !ok {
		state = &State{}
		s.SetModuleState(m.Name(), state)
	}
	m.state = state.(*State)
}

func (m *Module) HandleMsg(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	var err error

	switch msg.Type {
	case hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST:
		err = m.handleParticipantJoin(ctx, respond, msg)

	case hagallpb.MsgType_MSG_TYPE_ENTITY_DELETE_REQUEST:
		err = m.handleEntityDelete(ctx, respond, msg)

	default:
		switch vikjapb.MsgType(msg.Type.Number()) {
		case vikjapb.MsgType_MSG_TYPE_VIKJA_ENTITY_ACTION_REQUEST:
			err = m.handleSetEntityAction(ctx, respond, msg)

		default:
			err = hwebsocket.ErrModuleMsgSkip
		}
	}

	return err
}

func (m *Module) HandleDisconnect() {
	participant := m.currentParticipant
	if participant == nil {
		return
	}

	for entityID := range participant.EntityIDs() {
		if entity, ok := m.currentSession.EntityByID(entityID); !ok || !entity.Persist {
			m.state.RemoveEntityActions(entityID)
		}
	}
}

func (m *Module) handleParticipantJoin(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	respond.Send(&vikjapb.State{
		Type:          vikjapb.MsgType_MSG_TYPE_VIKJA_STATE,
		Timestamp:     timestamppb.Now(),
		EntityActions: m.state.EntityActions(),
	})
	return nil
}

func (m *Module) handleEntityDelete(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	var req hagallpb.EntityDeleteRequest
	if err := msg.DataTo(&req); err != nil {
		return err
	}

	if _, ok := m.currentSession.EntityByID(req.EntityId); !ok {
		m.state.RemoveEntityActions(req.EntityId)
	}

	return nil
}

func (m *Module) handleSetEntityAction(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	var req vikjapb.EntityActionRequest
	if err := msg.DataTo(&req); err != nil {
		return err
	}

	session := m.currentSession
	participant := m.currentParticipant
	if session == nil || participant == nil {
		return errors.New("session not joined").
			WithType(hwebsocket.ErrTypeSessionNotJoined).
			WithTag("msg_type", msg.Type)
	}

	entityAction := req.EntityAction

	if entityAction == nil ||
		entityAction.Name == "" ||
		entityAction.Timestamp == nil {
		respond.Send(&hagallpb.ErrorResponse{
			Type:      hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE,
			Timestamp: timestamppb.Now(),
			RequestId: req.RequestId,
			Code:      hagallpb.ErrorCode_ERROR_CODE_BAD_REQUEST,
		})
		return nil
	}

	if _, ok := session.EntityByID(entityAction.EntityId); !ok {
		respond.Send(&hagallpb.ErrorResponse{
			Type:      hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE,
			Timestamp: timestamppb.Now(),
			RequestId: req.RequestId,
			Code:      hagallpb.ErrorCode_ERROR_CODE_BAD_REQUEST,
		})
		return nil
	}

	latestEntityAction, ok := m.state.EntityAction(entityAction.EntityId, entityAction.Name)
	if ok && entityAction.Timestamp.AsTime().Before(latestEntityAction.Timestamp.AsTime()) {
		respond.Send(&hagallpb.ErrorResponse{
			Type:      hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE,
			Timestamp: timestamppb.Now(),
			RequestId: req.RequestId,
			Code:      hagallpb.ErrorCode_ERROR_CODE_BAD_REQUEST,
		})
		return nil
	}

	m.state.SetEntityAction(entityAction)

	now := timestamppb.Now()
	respond.Send(&vikjapb.EntityActionResponse{
		Type:      vikjapb.MsgType_MSG_TYPE_VIKJA_ENTITY_ACTION_RESPONSE,
		Timestamp: now,
		RequestId: req.RequestId,
	})
	session.Broadcast(participant, &vikjapb.EntityActionBroadcast{
		Type:            vikjapb.MsgType_MSG_TYPE_VIKJA_ENTITY_ACTION_BROADCAST,
		Timestamp:       now,
		OriginTimestamp: req.Timestamp,
		EntityAction:    entityAction,
	})
	return nil
}
