package websocket

import (
	"context"
	"sync"
	"time"

	"github.com/aukilabs/go-tooling/pkg/errors"
	"github.com/aukilabs/go-tooling/pkg/logs"
	"github.com/aukilabs/hagall-common/messages/hagallpb"
	hwebsocket "github.com/aukilabs/hagall-common/websocket"
	"github.com/aukilabs/hagall/models"
	"github.com/aukilabs/hagall/modules"
	"golang.org/x/net/websocket"
)

const (
	sendChanSize = 512
)

// Handler represents a hagall handler.
type Handler interface {
	// Handles a ping request.
	HandlePing(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error

	HandlePingResponse(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error

	// Handles a signed ping request.
	HandleSignedLatency(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error

	// Handles a client connection.
	HandleConnect(conn *websocket.Conn)

	// Handles a request to join a session.
	HandleParticipantJoin(ctx context.Context, handleFrame func(), sender hwebsocket.ResponseSender, msg hwebsocket.Msg) error

	// Handles a client's disconnection.
	HandleDisconnect(error)

	// Handles a request to create an entity.
	HandleEntityAdd(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error

	// Handles a request to delete an entity and its associated resources.
	HandleEntityDelete(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error

	// Handles an entity pose update.
	HandleEntityUpdatePose(ctx context.Context, msg hwebsocket.Msg) error

	// Handles a custom message.
	HandleCustomMessage(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error

	// Handles a request to add an entity component type.
	HandleEntityComponentTypeAdd(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error

	// Handles a request to get the name of an entity component.
	HandleEntityComponentGetName(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error

	// Handles a request to get the id of an entity component.
	HandleEntityComponentGetID(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error

	// handles a request to add an entity component.
	HandleEntityComponentAdd(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error

	// Handles a request to delete an entity component.
	HandleEntityComponentDelete(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error

	// Handles a request to update an entity component.
	HandleEntityComponentUpdate(ctx context.Context, msg hwebsocket.Msg) error

	// Handles a request to list entity components.
	HandleEntityComponentList(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error

	// Handles a request to subscribe to event related to a registered entity
	// component.
	HandleEntityComponentSubscribe(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error

	// Handles a request to unsubscribe to an entity component.
	HandleEntityComponentUnsubscribe(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error

	// Handles a request to pass the proof of work receipt to network credit service.
	HandleReceipt(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error

	// Handle a message with a module.
	HandleWithModule(ctx context.Context, module modules.Module, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error

	// Sends a sync clock message to the client.
	SendSyncClock(ctx context.Context, send hwebsocket.ResponseSender) error

	// Creates a message receiver used to receive incoming messages.
	Receiver() hwebsocket.Receiver

	// Creates a message sender passed in service methods in order to send
	// messages.
	Sender() hwebsocket.Sender

	// Closes the service and releases its allocated resources.
	Close()

	// The interval between each sync clock message sent to the connected
	// client.
	SyncClockInterval() time.Duration

	// The time a client is idle before being disconnected.
	IdleTimeout() time.Duration

	// Returns the session store.
	GetSessions() *models.SessionStore

	// Returns the modules.
	GetModules() []modules.Module

	// The currently joined session.
	CurrentSession() *models.Session

	// The current participant.
	CurrentParticipant() *models.Participant

	// Get ClientID
	GetClientID() string
}

// Handle handles the given service.
func Handle(ctx context.Context, conn *websocket.Conn, h Handler) {
	handler := handler{
		Conn:    conn,
		Handler: h,
	}

	handler.Handle(ctx)
}

type handler struct {
	// The WebSocket connection.
	Conn *websocket.Conn

	// The Hagall handler.
	Handler Handler

	sendChan       chan hwebsocket.Msg
	sender         hwebsocket.Sender
	dispatcher     hwebsocket.Dispatcher
	consumer       hwebsocket.Consumer
	receiver       hwebsocket.Receiver
	disconnectChan chan error
}

func (h *handler) Handle(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	h.Handler.HandleConnect(h.Conn)

	h.disconnectChan = make(chan error, 8)
	defer func() {
		for len(h.disconnectChan) != 0 {
			<-h.disconnectChan
		}
	}()

	var wg sync.WaitGroup

	h.sendChan = make(chan hwebsocket.Msg, sendChanSize)
	h.sender = h.Handler.Sender()

	wg.Add(1)
	go func() {
		defer wg.Done()
		h.startSending(ctx)
	}()

	scheduler := hwebsocket.NewScheduler()
	h.dispatcher = scheduler
	h.consumer = scheduler
	defer scheduler.Close()

	h.receiver = h.Handler.Receiver()
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.startReceiving(ctx)
	}()

	idleTimeout := h.Handler.IdleTimeout()
	idleTimer := time.NewTimer(idleTimeout)
	defer idleTimer.Stop()

	syncClockTicker := time.NewTicker(h.Handler.SyncClockInterval())
	defer syncClockTicker.Stop()

	var responder = responseSender{
		send:    h.send,
		sendMsg: h.sendMsg,
	}

	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			h.disconnect(ctx.Err())

		case <-idleTimer.C:
			h.disconnect(errors.New("idle connection").WithTag("duration", h.Handler.IdleTimeout()))

		case <-syncClockTicker.C:
			if err := h.Handler.SendSyncClock(ctx, responder); err != nil {
				h.disconnect(errors.New("sending sync clock failed").Wrap(err))
			}

		case msg := <-h.consumer.Messages():
			idleTimer.Stop()
			idleTimer.Reset(idleTimeout)

			if err := h.handleMessage(ctx, msg, responder); err != nil {
				h.disconnect(errors.New("handling message failed").Wrap(err))
			}

		case err := <-h.disconnectChan:
			h.handleDisconnect(err)
			if ctx.Err() == nil {
				// cancel context so go routines can cleanly exit
				cancel()
			}
		}
	}

	wg.Wait()
}

func (h *handler) send(protoMsg hwebsocket.ProtoMsg) {
	msg, err := hwebsocket.MsgFromProto(protoMsg)
	if err != nil {
		logs.WithTag("message", protoMsg).
			WithClientID(h.Handler.GetClientID()).
			Debug(err)
		return
	}
	h.sendChan <- msg
}

func (h *handler) sendMsg(msg hwebsocket.Msg) {
	h.sendChan <- msg
}

func (h *handler) startSending(ctx context.Context) {
	defer func() {
		for len(h.sendChan) != 0 {
			<-h.sendChan
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return

		case msg := <-h.sendChan:
			if _, err := h.sender(msg); err != nil {
				h.disconnect(errors.New("sending message failed").Wrap(err))
				return
			}
		}
	}
}

func (h *handler) startReceiving(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return

		default:
			msg, _, err := h.receiver()
			if err != nil {
				h.disconnect(errors.New("receiving message failed").Wrap(err))
				return
			}

			if err = h.dispatcher.Dispatch(ctx, msg); err != nil {
				h.disconnect(errors.New("dispatching message failed").Wrap(err))
				return
			}
		}
	}
}

func (h *handler) handleMessage(ctx context.Context, msg hwebsocket.Msg, responder hwebsocket.ResponseSender) error {
	var err error

	switch msg.Type {
	case hagallpb.MsgType_MSG_TYPE_PING_REQUEST:
		err = h.Handler.HandlePing(ctx, responder, msg)

	case hagallpb.MsgType_MSG_TYPE_PING_RESPONSE:
		err = h.Handler.HandlePingResponse(ctx, responder, msg)

	case hagallpb.MsgType_MSG_TYPE_SIGNED_LATENCY_REQUEST:
		err = h.Handler.HandleSignedLatency(ctx, responder, msg)

	case hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST:
		err = h.Handler.HandleParticipantJoin(ctx,
			h.dispatcher.HandleFrame,
			responder,
			msg,
		)

	case hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_REQUEST:
		err = h.Handler.HandleEntityAdd(ctx, responder, msg)

	case hagallpb.MsgType_MSG_TYPE_ENTITY_DELETE_REQUEST:
		err = h.Handler.HandleEntityDelete(ctx, responder, msg)

	case hagallpb.MsgType_MSG_TYPE_ENTITY_UPDATE_POSE:
		err = h.Handler.HandleEntityUpdatePose(ctx, msg)

	case hagallpb.MsgType_MSG_TYPE_CUSTOM_MESSAGE:
		err = h.Handler.HandleCustomMessage(ctx, responder, msg)

	case hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_ADD_REQUEST:
		err = h.Handler.HandleEntityComponentTypeAdd(ctx, responder, msg)

	case hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_GET_NAME_REQUEST:
		err = h.Handler.HandleEntityComponentGetName(ctx, responder, msg)

	case hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_GET_ID_REQUEST:
		err = h.Handler.HandleEntityComponentGetID(ctx, responder, msg)

	case hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_ADD_REQUEST:
		err = h.Handler.HandleEntityComponentAdd(ctx, responder, msg)

	case hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_DELETE_REQUEST:
		err = h.Handler.HandleEntityComponentDelete(ctx, responder, msg)

	case hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_LIST_REQUEST:
		err = h.Handler.HandleEntityComponentList(ctx, responder, msg)

	case hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_UPDATE:
		err = h.Handler.HandleEntityComponentUpdate(ctx, msg)

	case hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_SUBSCRIBE_REQUEST:
		err = h.Handler.HandleEntityComponentSubscribe(ctx, responder, msg)

	case hagallpb.MsgType_MSG_TYPE_ENTITY_COMPONENT_TYPE_UNSUBSCRIBE_REQUEST:
		err = h.Handler.HandleEntityComponentUnsubscribe(ctx, responder, msg)

	case hagallpb.MsgType_MSG_TYPE_RECEIPT_REQUEST:
		err = h.Handler.HandleReceipt(ctx, responder, msg)
	}

	if err != nil {
		return err
	}

	if h.Handler.CurrentParticipant() == nil || h.Handler.CurrentSession() == nil {
		return nil
	}

	for _, m := range h.Handler.GetModules() {
		if err = h.Handler.HandleWithModule(ctx, m, responder, msg); err != nil {
			return err
		}
	}
	return nil
}

func (h *handler) disconnect(err error) {
	h.disconnectChan <- err
}

func (h *handler) handleDisconnect(err error) {
	h.Conn.Close()
	h.Handler.HandleDisconnect(err)
}

type responseSender struct {
	send    func(hwebsocket.ProtoMsg)
	sendMsg func(hwebsocket.Msg)
}

func (r responseSender) Send(protoMsg hwebsocket.ProtoMsg) {
	r.send(protoMsg)
}

func (r responseSender) SendMsg(msg hwebsocket.Msg) {
	r.sendMsg(msg)
}
