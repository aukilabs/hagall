package websocket

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/aukilabs/hagall-common/errors"
	httpcmn "github.com/aukilabs/hagall-common/http"
	"github.com/aukilabs/hagall-common/logs"
	"github.com/aukilabs/hagall-common/messages/hagallpb"
	hwebsocket "github.com/aukilabs/hagall-common/websocket"
	"golang.org/x/net/websocket"
)

type Logger func(format string, v ...interface{})

func HandlerWithLogs(h Handler, summaryInterval time.Duration) Handler {
	ctx, cancel := context.WithCancel(context.Background())

	handler := &handlerWithLogs{
		Handler:            h,
		summaryInterval:    summaryInterval,
		closeSummaryWorker: cancel,
		counter:            make(map[string]int),
	}

	go handler.startSummaryWorker(ctx)
	return handler
}

type handlerWithLogs struct {
	Handler

	originalRequest *http.Request
	appKey          string

	summaryInterval    time.Duration
	closeSummaryWorker func()
	counterMutex       sync.Mutex
	counter            map[string]int

	sessionID     string
	sessionUUID   string
	participantID uint32
}

func (h *handlerWithLogs) HandleConnect(conn *websocket.Conn) {
	h.Handler.HandleConnect(conn)

	req := conn.Request()
	h.originalRequest = req
	h.appKey = httpcmn.GetAppKeyFromHagallUserToken(httpcmn.GetUserTokenFromHTTPRequest(req))

	logs.WithClientID(h.GetClientID()).
		WithTag(logs.AppKeyTag, h.appKey).
		Info("new client is connected")
}

func (h *handlerWithLogs) HandleParticipantJoin(ctx context.Context, handleFrame func(), sender hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	if err := h.Handler.HandleParticipantJoin(ctx, handleFrame, sender, msg); err != nil {
		return err
	}

	if h.CurrentParticipant() == nil {
		var req hagallpb.ParticipantJoinRequest
		// Check for error here is unecessary since it would never go here
		// if the request parsing failed in h.Handler.HandleParticipantJoin.
		msg.DataTo(&req)

		logs.WithClientID(h.GetClientID()).
			WithTag(logs.AppKeyTag, h.appKey).
			WithTag(logs.SessionIDTag, req.SessionId).
			WithTag("request_id", req.RequestId).
			WithTag("http_headers", struct {
				UserAgent               string `json:"user_agent,omitempty"`
				XForwardedFor           string `json:"x_forwarded_for,omitempty"`
				CloudFrontCountryName   string `json:"cloudfront_viewer_country,omitempty"`
				CloudFrontViewerAddress string `json:"cloudfront_viewer_address,omitempty"`
			}{
				UserAgent:               h.originalRequest.UserAgent(),
				XForwardedFor:           h.originalRequest.Header.Get(httpcmn.XForwardedForHeaderKey),
				CloudFrontCountryName:   h.originalRequest.Header.Get(httpcmn.CloudFrontCountryNameHeaderKey),
				CloudFrontViewerAddress: h.originalRequest.Header.Get(httpcmn.CloudFrontViewerAddressHeaderKey),
			}).
			Info("participant failed to join a session")
		return nil
	}

	h.sessionID = h.GetSessions().GlobalSessionID(h.CurrentSession().ID)
	h.sessionUUID = h.CurrentSession().SessionUUID
	h.participantID = h.CurrentParticipant().ID

	logs.WithClientID(h.GetClientID()).
		WithTag(logs.AppKeyTag, h.appKey).
		WithTag(logs.SessionIDTag, h.sessionID).
		WithTag("session_uuid", h.sessionUUID).
		WithTag(logs.ParticipantIDTag, h.participantID).
		WithTag("http_headers", struct {
			UserAgent               string `json:"user_agent,omitempty"`
			XForwardedFor           string `json:"x_forwarded_for,omitempty"`
			CloudFrontCountryName   string `json:"cloudfront_viewer_country,omitempty"`
			CloudFrontViewerAddress string `json:"cloudfront_viewer_address,omitempty"`
		}{
			UserAgent:               h.originalRequest.UserAgent(),
			XForwardedFor:           h.originalRequest.Header.Get(httpcmn.XForwardedForHeaderKey),
			CloudFrontCountryName:   h.originalRequest.Header.Get(httpcmn.CloudFrontCountryNameHeaderKey),
			CloudFrontViewerAddress: h.originalRequest.Header.Get(httpcmn.CloudFrontViewerAddressHeaderKey),
		}).
		Info("participant joined a session")
	return nil
}

func (h *handlerWithLogs) HandleDisconnect(err error) {
	h.Handler.HandleDisconnect(err)
	logs.WithClientID(h.GetClientID()).
		WithTag(logs.AppKeyTag, h.appKey).
		WithTag(logs.SessionIDTag, h.sessionID).
		WithTag(logs.ParticipantIDTag, h.participantID).
		Info("client disconnected")
}

func (h *handlerWithLogs) Receiver() hwebsocket.Receiver {
	receive := h.Handler.Receiver()

	return func() (hwebsocket.Msg, int, error) {
		msg, n, err := receive()
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			logs.WithClientID(h.GetClientID()).
				WithTag(logs.AppKeyTag, h.appKey).
				WithTag(logs.SessionIDTag, h.sessionID).
				WithTag("session_uuid", h.sessionUUID).
				WithTag(logs.ParticipantIDTag, h.participantID).
				Error(errors.New("receiving message failed").Wrap(err))
		} else if err == nil {
			logs.WithClientID(h.GetClientID()).
				WithTag(logs.AppKeyTag, h.appKey).
				WithTag(logs.SessionIDTag, h.sessionID).
				WithTag("session_uuid", h.sessionUUID).
				WithTag(logs.ParticipantIDTag, h.participantID).
				WithTag("msg_type", msg.TypeString()).
				Debug("message received")
			h.incCounter(msg.TypeString())
		}
		return msg, n, err
	}

}

func (h *handlerWithLogs) Sender() hwebsocket.Sender {
	sender := h.Handler.Sender()

	return func(msg hwebsocket.Msg) (int, error) {
		msgType := msg.TypeString()

		n, err := sender(msg)
		if err != nil && !errors.Is(err, net.ErrClosed) {
			logs.WithClientID(h.GetClientID()).
				WithTag(logs.AppKeyTag, h.appKey).
				WithTag(logs.SessionIDTag, h.sessionID).
				WithTag("session_uuid", h.sessionUUID).
				WithTag(logs.ParticipantIDTag, h.participantID).
				WithTag("msg_type", msgType).
				Error(errors.New("sending message failed").Wrap(err))
		} else if err == nil {
			logs.WithClientID(h.GetClientID()).
				WithTag(logs.AppKeyTag, h.appKey).
				WithTag(logs.SessionIDTag, h.sessionID).
				WithTag("session_uuid", h.sessionUUID).
				WithTag(logs.ParticipantIDTag, h.participantID).
				WithTag("msg_type", msgType).
				Debug("message sent")
		}
		return n, err
	}
}

func (h *handlerWithLogs) Close() {
	h.Handler.Close()
	h.closeSummaryWorker()
	h.logSummary()
}

func (h *handlerWithLogs) startSummaryWorker(ctx context.Context) {
	ticker := time.NewTicker(h.summaryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			h.logSummary()
		}
	}
}

func (h *handlerWithLogs) incCounter(msgType string) {
	h.counterMutex.Lock()
	defer h.counterMutex.Unlock()

	h.counter[msgType]++
}

func (h *handlerWithLogs) logSummary() {
	h.counterMutex.Lock()
	defer h.counterMutex.Unlock()

	if len(h.counter) == 0 {
		return
	}

	entry := logs.
		WithClientID(h.GetClientID()).
		WithTag(logs.AppKeyTag, h.appKey).
		WithTag(logs.ParticipantIDTag, h.participantID).
		WithTag(logs.SessionIDTag, h.sessionID).
		WithTag("session_uuid", h.sessionUUID).
		WithTag("time_interval", h.summaryInterval)

	for k, v := range h.counter {
		entry = entry.WithTag(k, v)
		delete(h.counter, k)
	}

	entry.Info("inbound message summary")
}
