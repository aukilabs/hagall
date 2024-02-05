package odal

import (
	"context"

	"github.com/aukilabs/hagall-common/errors"
	"github.com/aukilabs/hagall-common/messages/hagallpb"
	"github.com/aukilabs/hagall-common/messages/odalpb"
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
	return "odal"
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
		switch odalpb.MsgType(msg.Type.Number()) {
		case odalpb.MsgType_MSG_TYPE_ODAL_ASSET_INSTANCE_ADD_REQUEST:
			err = m.handleAssetInstanceAdd(ctx, respond, msg)

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
			m.state.RemoveAssetInstance(entityID)
		}
	}
}

func (m *Module) handleParticipantJoin(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	respond.Send(&odalpb.State{
		Type:           odalpb.MsgType_MSG_TYPE_ODAL_STATE,
		Timestamp:      timestamppb.Now(),
		AssetInstances: m.state.AssetInstances(),
	})
	return nil
}

func (m *Module) handleEntityDelete(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	var req hagallpb.EntityDeleteRequest
	if err := msg.DataTo(&req); err != nil {
		return err
	}

	if _, ok := m.currentSession.EntityByID(req.EntityId); !ok {
		m.state.RemoveAssetInstance(req.EntityId)
	}

	return nil
}

func (m *Module) handleAssetInstanceAdd(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	var req odalpb.AssetInstanceAddRequest
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

	if req.AssetId == "" {
		respond.Send(&hagallpb.ErrorResponse{
			Type:      hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE,
			Timestamp: timestamppb.Now(),
			RequestId: req.RequestId,
			Code:      hagallpb.ErrorCode_ERROR_CODE_BAD_REQUEST,
		})
		return nil
	}

	entity, ok := session.EntityByID(req.EntityId)
	if !ok {
		respond.Send(&hagallpb.ErrorResponse{
			Type:      hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE,
			Timestamp: timestamppb.Now(),
			RequestId: req.RequestId,
			Code:      hagallpb.ErrorCode_ERROR_CODE_NOT_FOUND,
		})
		return nil
	}

	if entity.ParticipantID != participant.ID {
		respond.Send(&hagallpb.ErrorResponse{
			Type:      hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE,
			Timestamp: timestamppb.Now(),
			RequestId: req.RequestId,
			Code:      hagallpb.ErrorCode_ERROR_CODE_UNAUTHORIZED,
		})
		return nil
	}

	assetInstance := &odalpb.AssetInstance{
		Id:            m.state.NewAssetInstanceID(),
		AssetId:       req.AssetId,
		ParticipantId: participant.ID,
		EntityId:      entity.ID,
	}
	m.state.SetAssetInstance(assetInstance)

	now := timestamppb.Now()
	respond.Send(&odalpb.AssetInstanceAddResponse{
		Type:            odalpb.MsgType_MSG_TYPE_ODAL_ASSET_INSTANCE_ADD_RESPONSE,
		Timestamp:       now,
		RequestId:       req.RequestId,
		AssetInstanceId: assetInstance.Id,
	})
	session.Broadcast(participant, &odalpb.AssetInstanceAddBroadcast{
		Type:            odalpb.MsgType_MSG_TYPE_ODAL_ASSET_INSTANCE_ADD_BROADCAST,
		Timestamp:       now,
		OriginTimestamp: req.Timestamp,
		AssetInstance:   assetInstance,
	})
	return nil
}
