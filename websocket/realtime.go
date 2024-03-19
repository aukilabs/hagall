package websocket

import (
	"context"
	"time"

	"github.com/aukilabs/go-tooling/pkg/errors"
	httpcmn "github.com/aukilabs/hagall-common/http"
	"github.com/aukilabs/hagall-common/messages/hagallpb"
	"github.com/aukilabs/hagall-common/ncsclient"
	hwebsocket "github.com/aukilabs/hagall-common/websocket"
	"github.com/aukilabs/hagall/featureflag"
	"github.com/aukilabs/hagall/models"
	"github.com/aukilabs/hagall/modules"
	"golang.org/x/net/websocket"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const customMessageMaxSize = 10240

// RealtimeHandler represents a service that manages multiple client connections
// and relays their actions in realtime.
type RealtimeHandler struct {
	// The interval between each sync clock message sent to the connected
	// client.
	ClientSyncClockInterval time.Duration

	// The time a client is idle before being disconnected.
	ClientIdleTimeout time.Duration

	// The duration of a frame.
	FrameDuration time.Duration

	// The store that contains all the server sessions.
	Sessions *models.SessionStore

	// The module that expand Hagall features.
	Modules []modules.Module

	FeatureFlags featureflag.FeatureFlag

	// channel for sending incoming receipts to ReceiptHandler goroutine
	ReceiptChan chan ncsclient.ReceiptPayload

	conn               *websocket.Conn
	currentSession     *models.Session
	currentParticipant *models.Participant

	stopFrameHandling func()

	clientID string
	appKey   string
}

func (h *RealtimeHandler) HandleConnect(conn *websocket.Conn) {
	req := conn.Request()
	h.clientID = req.Header.Get(httpcmn.HeaderPosemeshClientID)
	h.appKey = httpcmn.GetAppKeyFromHagallUserToken(httpcmn.GetUserTokenFromHTTPRequest(req))

	h.conn = conn
}

func (h *RealtimeHandler) HandlePing(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	var req hagallpb.Request
	if err := msg.DataTo(&req); err != nil {
		return err
	}

	respond.Send(&hagallpb.Response{
		Type:      hagallpb.MsgType_MSG_TYPE_PING_RESPONSE,
		Timestamp: timestamppb.Now(),
		RequestId: req.RequestId,
	})
	return nil
}

func (h *RealtimeHandler) HandleSignedPing(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	var req hagallpb.Request
	if err := msg.DataTo(&req); err != nil {
		return err
	}

	respond.Send(&hagallpb.Response{
		Type:      hagallpb.MsgType_MSG_TYPE_SIGNED_PING_RESPONSE,
		Timestamp: timestamppb.Now(),
		RequestId: req.RequestId,
	})
	return nil
}

func (h *RealtimeHandler) HandleParticipantJoin(ctx context.Context, handleFrame func(), respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	var req hagallpb.ParticipantJoinRequest
	if err := msg.DataTo(&req); err != nil {
		return err
	}

	if h.currentSession != nil && h.Sessions.GlobalSessionID(h.currentSession.ID) == req.SessionId {
		respond.Send(&hagallpb.ErrorResponse{
			Type:      hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE,
			Timestamp: timestamppb.Now(),
			RequestId: req.RequestId,
			Code:      hagallpb.ErrorCode_ERROR_CODE_SESSION_ALREADY_JOINED,
		})
		return nil
	}

	if h.currentParticipant != nil {
		h.leaveSession()
	}

	session, ok := h.Sessions.GetByGlobalID(req.SessionId)
	if !ok && req.SessionId != "" {
		respond.Send(&hagallpb.ErrorResponse{
			Type:      hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE,
			Timestamp: timestamppb.Now(),
			RequestId: req.RequestId,
			Code:      hagallpb.ErrorCode_ERROR_CODE_NOT_FOUND,
		})
		return nil
	}

	if !ok {
		session = models.NewSession(h.Sessions.NewID(), h.FrameDuration)
		session.AppKey = h.appKey
		if err := h.Sessions.Add(ctx, session); err != nil {
			respond.Send(&hagallpb.ErrorResponse{
				Type:      hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE,
				Timestamp: timestamppb.Now(),
				RequestId: req.RequestId,
				Code:      hagallpb.ErrorCode_ERROR_CODE_INTERNAL_SERVER_ERROR,
			})
			return nil
		}
		go session.StartDispatchFrames()
	}

	participant := &models.Participant{
		ID:        session.NewParticipantID(),
		Responder: respond,
	}

	session.AddParticipant(participant)
	h.stopFrameHandling = session.HandleFrame(handleFrame)

	respond.Send(&hagallpb.ParticipantJoinResponse{
		Type:          hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE,
		Timestamp:     timestamppb.Now(),
		RequestId:     req.RequestId,
		SessionId:     h.Sessions.GlobalSessionID(session.ID),
		SessionUuid:   session.SessionUUID,
		ParticipantId: participant.ID,
	})

	h.currentSession = session
	h.currentParticipant = participant

	h.FeatureFlags.IfNotSet(featureflag.FlagDisableSessionState, func() {
		respond.Send(&hagallpb.SessionState{
			Type:             hagallpb.MsgType_MSG_TYPE_SESSION_STATE,
			Timestamp:        timestamppb.Now(),
			Participants:     models.ParticipantsToProtobuf(session.GetParticipants()),
			Entities:         models.EntitiesToProtobuf(session.Entities()),
			EntityComponents: session.GetEntityComponents().ListAll(),
		})
	})

	h.FeatureFlags.IfNotSet(featureflag.FlagDisableParticipantJoinBroadcast, func() {
		session.Broadcast(participant, &hagallpb.ParticipantJoinBroadcast{
			Type:            hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_BROADCAST,
			Timestamp:       timestamppb.Now(),
			OriginTimestamp: req.Timestamp,
			ParticipantId:   participant.ID,
		})
	})

	for _, m := range h.Modules {
		m.Init(session, participant)
	}

	return nil
}

func (h *RealtimeHandler) HandleDisconnect(_ error) {
	if h.currentParticipant != nil {
		h.leaveSession()
	}
}

func (h *RealtimeHandler) HandleEntityAdd(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	var req hagallpb.EntityAddRequest
	if err := msg.DataTo(&req); err != nil {
		return err
	}

	participant := h.currentParticipant
	session := h.currentSession
	if participant == nil || session == nil {
		return errors.New("session not joined").
			WithType(hwebsocket.ErrTypeSessionNotJoined).
			WithTag("msg_type", msg.Type)
	}

	entity := &models.Entity{
		ID:            session.NewEntityID(),
		ParticipantID: participant.ID,
		Persist:       req.Persist,
		Flag:          req.Flag,
	}

	if req.Pose != nil {
		entity.SetPose(models.Pose{
			PX: req.Pose.Px,
			PY: req.Pose.Py,
			PZ: req.Pose.Pz,
			RX: req.Pose.Rx,
			RY: req.Pose.Ry,
			RZ: req.Pose.Rz,
			RW: req.Pose.Rw,
		})
	}

	session.AddEntity(entity)
	participant.AddEntity(entity)

	now := timestamppb.Now()

	respond.Send(&hagallpb.EntityAddResponse{
		Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_RESPONSE,
		Timestamp: now,
		RequestId: req.RequestId,
		EntityId:  entity.ID,
	})

	h.FeatureFlags.IfNotSet(featureflag.FlagDisableEntityAddBroadcast, func() {
		session.Broadcast(participant, &hagallpb.EntityAddBroadcast{
			Type:            hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_BROADCAST,
			Timestamp:       now,
			OriginTimestamp: req.Timestamp,
			Entity:          entity.ToProtobuf(),
		})
	})

	return nil
}

func (h *RealtimeHandler) HandleEntityDelete(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	var req hagallpb.EntityDeleteRequest
	if err := msg.DataTo(&req); err != nil {
		return err
	}

	participant := h.currentParticipant
	session := h.currentSession
	if participant == nil || session == nil {
		return errors.New("session not joined").
			WithType(hwebsocket.ErrTypeSessionNotJoined).
			WithTag("msg_type", msg.Type)
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

	now := timestamppb.Now()

	session.GetEntityComponents().DeleteByEntityID(entity.ID)
	session.RemoveEntity(entity)
	participant.RemoveEntity(entity)

	respond.Send(&hagallpb.EntityDeleteResponse{
		Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_DELETE_RESPONSE,
		Timestamp: now,
		RequestId: req.RequestId,
	})

	h.FeatureFlags.IfNotSet(featureflag.FlagDisableEntityDeleteBroadcast, func() {
		session.Broadcast(participant, &hagallpb.EntityDeleteBroadcast{
			Type:            hagallpb.MsgType_MSG_TYPE_ENTITY_DELETE_BROADCAST,
			Timestamp:       now,
			OriginTimestamp: req.Timestamp,
			EntityId:        entity.ID,
		})
	})

	return nil
}

func (h *RealtimeHandler) HandleEntityUpdatePose(ctx context.Context, msg hwebsocket.Msg) error {
	var update hagallpb.EntityUpdatePose
	if err := msg.DataTo(&update); err != nil {
		return err
	}

	participant := h.currentParticipant
	session := h.currentSession
	if participant == nil || session == nil {
		return errors.New("session not joined").
			WithType(hwebsocket.ErrTypeSessionNotJoined).
			WithTag("msg_type", msg.Type)
	}

	entity, ok := session.EntityByID(update.EntityId)
	if !ok {
		return nil
	}

	if entity.ParticipantID != participant.ID {
		return nil
	}

	entity.SetPose(models.Pose{
		PX: update.Pose.Px,
		PY: update.Pose.Py,
		PZ: update.Pose.Pz,
		RX: update.Pose.Rx,
		RY: update.Pose.Ry,
		RZ: update.Pose.Rz,
		RW: update.Pose.Rw,
	})

	h.FeatureFlags.IfNotSet(featureflag.FlagDisableEntityUpdatePoseBroadcast, func() {
		session.Broadcast(participant, &hagallpb.EntityUpdatePoseBroadcast{
			Type:            hagallpb.MsgType_MSG_TYPE_ENTITY_UPDATE_POSE_BROADCAST,
			Timestamp:       timestamppb.Now(),
			OriginTimestamp: update.Timestamp,
			EntityId:        entity.ID,
			Pose:            entity.Pose().ToProtobuf(),
		})
	})

	return nil
}

func (h *RealtimeHandler) HandleCustomMessage(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	var customMessage hagallpb.CustomMessage
	if err := msg.DataTo(&customMessage); err != nil {
		return err
	}

	participant := h.currentParticipant
	session := h.currentSession
	if participant == nil || session == nil {
		return errors.New("session not joined").
			WithType(hwebsocket.ErrTypeSessionNotJoined).
			WithTag("msg_type", msg.Type)
	}

	if len(customMessage.Body) > customMessageMaxSize {
		respond.Send(&hagallpb.ErrorResponse{
			Type:      hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE,
			Timestamp: timestamppb.Now(),
			Code:      hagallpb.ErrorCode_ERROR_CODE_TOO_LARGE,
		})
		return nil
	}

	h.FeatureFlags.IfNotSet(featureflag.FlagDisableCustomMessageBroadcast, func() {
		customMessageBroadcast := hagallpb.CustomMessageBroadcast{
			Type:            hagallpb.MsgType_MSG_TYPE_CUSTOM_MESSAGE_BROADCAST,
			Timestamp:       timestamppb.Now(),
			OriginTimestamp: customMessage.Timestamp,
			ParticipantId:   participant.ID,
			Body:            customMessage.Body,
		}

		if len(customMessage.ParticipantIds) != 0 {
			session.BroadcastTo(participant, &customMessageBroadcast, customMessage.ParticipantIds...)
			return
		}

		session.Broadcast(participant, &customMessageBroadcast)
	})
	return nil
}

func (h *RealtimeHandler) HandleEntityComponentTypeAdd(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	var req hagallpb.EntityComponentTypeAddRequest
	if err := msg.DataTo(&req); err != nil {
		return err
	}

	if req.EntityComponentTypeName == "" {
		respond.Send(&hagallpb.ErrorResponse{
			Type:      hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE,
			Timestamp: timestamppb.Now(),
			RequestId: req.RequestId,
			Code:      hagallpb.ErrorCode_ERROR_CODE_BAD_REQUEST,
		})
		return nil
	}

	session := h.CurrentSession()
	if session == nil {
		return errors.New("session not joined").
			WithType(hwebsocket.ErrTypeSessionNotJoined).
			WithTag("msg_type", msg.Type)
	}

	respond.Send(&hagallpb.EntityComponentTypeAddResponse{
		Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_ADD_RESPONSE,
		Timestamp:             timestamppb.Now(),
		RequestId:             req.RequestId,
		EntityComponentTypeId: session.GetEntityComponents().AddType(req.EntityComponentTypeName),
	})
	return nil
}

func (h *RealtimeHandler) HandleEntityComponentGetName(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	var req hagallpb.EntityComponentTypeGetNameRequest
	if err := msg.DataTo(&req); err != nil {
		return err
	}

	if req.EntityComponentTypeId == 0 {
		respond.Send(&hagallpb.ErrorResponse{
			Type:      hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE,
			Timestamp: timestamppb.Now(),
			RequestId: req.RequestId,
			Code:      hagallpb.ErrorCode_ERROR_CODE_BAD_REQUEST,
		})
		return nil
	}

	session := h.CurrentSession()
	if session == nil {
		return errors.New("session not joined").
			WithType(hwebsocket.ErrTypeSessionNotJoined).
			WithTag("msg_type", msg.Type)
	}

	name, err := session.GetEntityComponents().GetTypeName(req.EntityComponentTypeId)
	if err != nil {
		respond.Send(&hagallpb.ErrorResponse{
			Type:      hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE,
			Timestamp: timestamppb.Now(),
			RequestId: req.RequestId,
			Code:      hagallpb.ErrorCode_ERROR_CODE_NOT_FOUND,
		})
		return nil
	}

	respond.Send(&hagallpb.EntityComponentTypeGetNameResponse{
		Type:                    hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_GET_NAME_RESPONSE,
		Timestamp:               timestamppb.Now(),
		RequestId:               req.RequestId,
		EntityComponentTypeName: name,
	})
	return nil
}

func (h *RealtimeHandler) HandleEntityComponentGetID(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	var req hagallpb.EntityComponentTypeGetIdRequest
	if err := msg.DataTo(&req); err != nil {
		return err
	}

	if req.EntityComponentTypeName == "" {
		respond.Send(&hagallpb.ErrorResponse{
			Type:      hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE,
			Timestamp: timestamppb.Now(),
			RequestId: req.RequestId,
			Code:      hagallpb.ErrorCode_ERROR_CODE_BAD_REQUEST,
		})
		return nil
	}

	session := h.CurrentSession()
	if session == nil {
		return errors.New("session not joined").
			WithType(hwebsocket.ErrTypeSessionNotJoined).
			WithTag("msg_type", msg.Type)
	}

	id, err := session.GetEntityComponents().GetTypeID(req.EntityComponentTypeName)
	if err != nil {
		respond.Send(&hagallpb.ErrorResponse{
			Type:      hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE,
			Timestamp: timestamppb.Now(),
			RequestId: req.RequestId,
			Code:      hagallpb.ErrorCode_ERROR_CODE_NOT_FOUND,
		})
		return nil
	}

	respond.Send(&hagallpb.EntityComponentTypeGetIdResponse{
		Type:                  hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_GET_ID_RESPONSE,
		Timestamp:             timestamppb.Now(),
		RequestId:             req.RequestId,
		EntityComponentTypeId: id,
	})
	return nil
}

func (h *RealtimeHandler) HandleEntityComponentAdd(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	var req hagallpb.EntityComponentAddRequest
	if err := msg.DataTo(&req); err != nil {
		return err
	}

	if req.EntityComponentTypeId == 0 || req.EntityId == 0 {
		respond.Send(&hagallpb.ErrorResponse{
			Type:      hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE,
			Timestamp: timestamppb.Now(),
			RequestId: req.RequestId,
			Code:      hagallpb.ErrorCode_ERROR_CODE_BAD_REQUEST,
		})
		return nil
	}

	session := h.CurrentSession()
	participant := h.CurrentParticipant()
	if session == nil || participant == nil {
		return errors.New("session not joined").
			WithType(hwebsocket.ErrTypeSessionNotJoined).
			WithTag("msg_type", msg.Type)
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

	entityComponent := hagallpb.EntityComponent{
		EntityComponentTypeId: req.EntityComponentTypeId,
		EntityId:              entity.ID,
		Data:                  req.Data,
	}

	if err := session.GetEntityComponents().Add(&entityComponent); err != nil {
		var errCode hagallpb.ErrorCode
		switch errors.Type(err) {
		case hwebsocket.ErrEntityComponentTypeAlreadyAdded:
			errCode = hagallpb.ErrorCode_ERROR_CODE_CONFLICT
		default:
			errCode = hagallpb.ErrorCode_ERROR_CODE_NOT_FOUND
		}

		respond.Send(&hagallpb.ErrorResponse{
			Type:      hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE,
			Timestamp: timestamppb.Now(),
			RequestId: req.RequestId,
			Code:      errCode,
		})
		return nil
	}

	now := timestamppb.Now()

	respond.Send(&hagallpb.EntityComponentAddResponse{
		Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_ADD_RESPONSE,
		Timestamp: now,
		RequestId: req.RequestId,
	})

	h.FeatureFlags.IfNotSet(featureflag.FlagDisableEntityComponentAddBroadcast, func() {
		session.GetEntityComponents().Notify(entityComponent.EntityComponentTypeId, func(participantIDs []uint32) {
			session.Broadcast(participant, &hagallpb.EntityComponentAddBroadcast{
				Type:            hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_ADD_BROADCAST,
				Timestamp:       now,
				OriginTimestamp: req.Timestamp,
				EntityComponent: &entityComponent,
			})
		})
	})

	return nil
}

func (h *RealtimeHandler) HandleEntityComponentDelete(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	var req hagallpb.EntityComponentDeleteRequest
	if err := msg.DataTo(&req); err != nil {
		return err
	}

	if req.EntityComponentTypeId == 0 || req.EntityId == 0 {
		respond.Send(&hagallpb.ErrorResponse{
			Type:      hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE,
			Timestamp: timestamppb.Now(),
			RequestId: req.RequestId,
			Code:      hagallpb.ErrorCode_ERROR_CODE_BAD_REQUEST,
		})
		return nil
	}

	session := h.CurrentSession()
	participant := h.CurrentParticipant()
	if session == nil || participant == nil {
		return errors.New("session not joined").
			WithType(hwebsocket.ErrTypeSessionNotJoined).
			WithTag("msg_type", msg.Type)
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

	if !session.GetEntityComponents().Delete(req.EntityComponentTypeId, entity.ID) {
		respond.Send(&hagallpb.ErrorResponse{
			Type:      hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE,
			Timestamp: timestamppb.Now(),
			RequestId: req.RequestId,
			Code:      hagallpb.ErrorCode_ERROR_CODE_NOT_FOUND,
		})
		return nil
	}

	h.FeatureFlags.IfNotSet(featureflag.FlagDisableEntityComponentDeleteBroadcast, func() {
		session.GetEntityComponents().Notify(req.EntityComponentTypeId, func(participantIDs []uint32) {
			session.Broadcast(participant, &hagallpb.EntityComponentDeleteBroadcast{
				Type:            hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_DELETE_BROADCAST,
				Timestamp:       timestamppb.Now(),
				OriginTimestamp: req.Timestamp,
				EntityComponent: &hagallpb.EntityComponent{
					EntityComponentTypeId: req.EntityComponentTypeId,
					EntityId:              entity.ID,
				},
			})
		})
	})

	respond.Send(&hagallpb.EntityComponentDeleteResponse{
		Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_DELETE_RESPONSE,
		Timestamp: timestamppb.Now(),
		RequestId: req.RequestId,
	})
	return nil
}

func (h *RealtimeHandler) HandleEntityComponentUpdate(ctx context.Context, msg hwebsocket.Msg) error {
	var req hagallpb.EntityComponentUpdate
	if err := msg.DataTo(&req); err != nil {
		return err
	}

	if req.EntityComponentTypeId == 0 || req.EntityId == 0 {
		return nil
	}

	session := h.CurrentSession()
	participant := h.CurrentParticipant()
	if session == nil || participant == nil {
		return errors.New("session not joined").
			WithType(hwebsocket.ErrTypeSessionNotJoined).
			WithTag("msg_type", msg.Type)
	}

	entity, ok := session.EntityByID(req.EntityId)
	if !ok {
		return nil
	}

	entityComponent := hagallpb.EntityComponent{
		EntityComponentTypeId: req.EntityComponentTypeId,
		EntityId:              entity.ID,
		Data:                  req.Data,
	}

	session.GetEntityComponents().Update(&entityComponent)

	h.FeatureFlags.IfNotSet(featureflag.FlagDisableEntityComponentUpdateBroadcast, func() {
		session.GetEntityComponents().Notify(entityComponent.EntityComponentTypeId, func(participantIDs []uint32) {
			session.BroadcastTo(participant, &hagallpb.EntityComponentUpdateBroadcast{
				Type:            hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_UPDATE_BROADCAST,
				Timestamp:       timestamppb.Now(),
				OriginTimestamp: req.Timestamp,
				EntityComponent: &entityComponent,
			}, participantIDs...)
		})
	})

	return nil
}

func (h *RealtimeHandler) HandleEntityComponentList(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	var req hagallpb.EntityComponentListRequest
	if err := msg.DataTo(&req); err != nil {
		return err
	}

	if req.EntityComponentTypeId == 0 {
		respond.Send(&hagallpb.ErrorResponse{
			Type:      hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE,
			Timestamp: timestamppb.Now(),
			RequestId: req.RequestId,
			Code:      hagallpb.ErrorCode_ERROR_CODE_BAD_REQUEST,
		})
		return nil
	}

	session := h.CurrentSession()
	if session == nil {
		return errors.New("session not joined").
			WithType(hwebsocket.ErrTypeSessionNotJoined).
			WithTag("msg_type", msg.Type)
	}

	respond.Send(&hagallpb.EntityComponentListResponse{
		Type:             hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_LIST_RESPONSE,
		Timestamp:        timestamppb.Now(),
		RequestId:        req.RequestId,
		EntityComponents: session.GetEntityComponents().List(req.EntityComponentTypeId),
	})
	return nil
}

func (h *RealtimeHandler) HandleEntityComponentSubscribe(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	var req hagallpb.EntityComponentTypeSubscribeRequest
	if err := msg.DataTo(&req); err != nil {
		return err
	}

	if req.EntityComponentTypeId == 0 {
		respond.Send(&hagallpb.ErrorResponse{
			Type:      hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE,
			Timestamp: timestamppb.Now(),
			RequestId: req.RequestId,
			Code:      hagallpb.ErrorCode_ERROR_CODE_BAD_REQUEST,
		})
		return nil
	}

	session := h.CurrentSession()
	participant := h.CurrentParticipant()
	if session == nil || participant == nil {
		return errors.New("session not joined").
			WithType(hwebsocket.ErrTypeSessionNotJoined).
			WithTag("msg_type", msg.Type)
	}

	if err := session.GetEntityComponents().Subscribe(req.EntityComponentTypeId, participant.ID); err != nil {
		respond.Send(&hagallpb.ErrorResponse{
			Type:      hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE,
			Timestamp: timestamppb.Now(),
			RequestId: req.RequestId,
			Code:      hagallpb.ErrorCode_ERROR_CODE_NOT_FOUND,
		})
		return nil
	}

	respond.Send(&hagallpb.EntityComponentTypeSubscribeResponse{
		Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_SUBSCRIBE_RESPONSE,
		Timestamp: timestamppb.Now(),
		RequestId: req.RequestId,
	})
	return nil
}

func (h *RealtimeHandler) HandleEntityComponentUnsubscribe(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	var req hagallpb.EntityComponentTypeUnsubscribeRequest
	if err := msg.DataTo(&req); err != nil {
		return err
	}

	if req.EntityComponentTypeId == 0 {
		respond.Send(&hagallpb.ErrorResponse{
			Type:      hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE,
			Timestamp: timestamppb.Now(),
			RequestId: req.RequestId,
			Code:      hagallpb.ErrorCode_ERROR_CODE_BAD_REQUEST,
		})
		return nil
	}

	session := h.CurrentSession()
	participant := h.CurrentParticipant()
	if session == nil || participant == nil {
		return errors.New("session not joined").
			WithType(hwebsocket.ErrTypeSessionNotJoined).
			WithTag("msg_type", msg.Type)
	}

	session.GetEntityComponents().Unsubscribe(req.EntityComponentTypeId, participant.ID)

	respond.Send(&hagallpb.EntityComponentTypeUnsubscribeResponse{
		Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_UNSUBSCRIBE_RESPONSE,
		Timestamp: timestamppb.Now(),
		RequestId: req.RequestId,
	})
	return nil
}

func (h *RealtimeHandler) HandleReceipt(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	var req hagallpb.ReceiptRequest
	if err := msg.DataTo(&req); err != nil {
		return err
	}

	if len(req.GetReceipt()) == 0 || len(req.GetHash()) == 0 || len(req.GetSignature()) == 0 {
		respond.Send(&hagallpb.ErrorResponse{
			Type:      hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE,
			Timestamp: timestamppb.Now(),
			RequestId: req.RequestId,
			Code:      hagallpb.ErrorCode_ERROR_CODE_BAD_REQUEST,
		})
		return errors.New("zero length receipt value detected")
	}

	payload := ncsclient.ReceiptPayload{
		Receipt:   req.GetReceipt(),
		Hash:      req.GetHash(),
		Signature: req.GetSignature(),
	}

	select {
	case h.ReceiptChan <- payload:
		respond.Send(&hagallpb.ReceiptResponse{
			Type:      hagallpb.MsgType_MSG_TYPE_RECEIPT_RESPONSE,
			Timestamp: timestamppb.Now(),
			RequestId: req.RequestId,
		})
	default:
		//discard - failsafe if disk is full or whatever
		respond.Send(&hagallpb.ErrorResponse{
			Type:      hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE,
			Timestamp: timestamppb.Now(),
			RequestId: req.RequestId,
			Code:      hagallpb.ErrorCode_ERROR_CODE_SERVER_TOO_BUSY,
		})
		return errors.New("ReceiptChan full")
	}

	return nil
}

func (h *RealtimeHandler) HandleWithModule(ctx context.Context, m modules.Module, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	if h.CurrentParticipant() == nil || h.CurrentSession() == nil {
		return nil
	}

	err := m.HandleMsg(ctx, respond, msg)
	if errors.IsType(err, hwebsocket.ErrTypeMsgSkip) {
		return nil
	}
	if err != nil {
		return errors.New("handling message with module failed").
			WithTag("module", m.Name()).
			Wrap(err)
	}
	return nil
}

func (h *RealtimeHandler) SendSyncClock(ctx context.Context, respond hwebsocket.ResponseSender) error {
	respond.Send(&hagallpb.SyncClock{
		Type:      hagallpb.MsgType_MSG_TYPE_SYNC_CLOCK,
		Timestamp: timestamppb.Now(),
	})
	return nil
}

func (h *RealtimeHandler) Receiver() hwebsocket.Receiver {
	return func() (hwebsocket.Msg, int, error) {
		return hwebsocket.Receive(h.conn)
	}
}

func (h *RealtimeHandler) Sender() hwebsocket.Sender {
	return func(msg hwebsocket.Msg) (int, error) {
		return hwebsocket.Send(h.conn, msg)
	}
}

func (h *RealtimeHandler) Close() {
}

func (h *RealtimeHandler) SyncClockInterval() time.Duration {
	return h.ClientSyncClockInterval
}

func (h *RealtimeHandler) IdleTimeout() time.Duration {
	return h.ClientIdleTimeout
}

func (h *RealtimeHandler) GetSessions() *models.SessionStore {
	return h.Sessions
}

func (h *RealtimeHandler) GetModules() []modules.Module {
	return h.Modules
}

func (h *RealtimeHandler) CurrentSession() *models.Session {
	return h.currentSession
}

func (h *RealtimeHandler) CurrentParticipant() *models.Participant {
	return h.currentParticipant
}

func (h *RealtimeHandler) leaveSession() {
	session := h.currentSession
	participant := h.currentParticipant

	if participant == nil || session == nil {
		return
	}

	for _, m := range h.Modules {
		m.HandleDisconnect()
	}

	session.GetEntityComponents().UnsubscribeByParticipant(participant.ID)

	now := timestamppb.Now()

	for id := range participant.EntityIDs() {
		entity, ok := session.EntityByID(id)
		if !ok || entity.Persist {
			continue
		}

		session.GetEntityComponents().DeleteByEntityID(entity.ID)
		session.RemoveEntity(entity)

		h.FeatureFlags.IfNotSet(featureflag.FlagDisableEntityDeleteBroadcast, func() {
			session.Broadcast(participant, &hagallpb.EntityDeleteBroadcast{
				Type:            hagallpb.MsgType_MSG_TYPE_ENTITY_DELETE_BROADCAST,
				Timestamp:       now,
				OriginTimestamp: now,
				EntityId:        entity.ID,
			})
		})
	}

	if h.stopFrameHandling != nil {
		h.stopFrameHandling()
	}
	session.RemoveParticipant(participant)

	h.FeatureFlags.IfNotSet(featureflag.FlagDisableParticipantLeaveBroadcast, func() {
		session.Broadcast(participant, &hagallpb.ParticipantLeaveBroadcast{
			Type:            hagallpb.MsgType_MSG_TYPE_PARTICIPANT_LEAVE_BROADCAST,
			Timestamp:       now,
			OriginTimestamp: now,
			ParticipantId:   participant.ID,
		})
	})

	if session.ParticipantCount() == 0 {
		// Here we use a context.Background to ensure the session to be deleted
		// on the session discovery service (eg HDS).
		h.Sessions.Remove(context.Background(), session)
		session.Close()
	}

	h.currentParticipant = nil
	h.currentSession = nil
}

func (h *RealtimeHandler) GetClientID() string {
	return h.clientID
}
