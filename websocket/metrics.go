package websocket

import (
	"context"
	"time"

	"github.com/aukilabs/go-tooling/pkg/errors"
	httpcmn "github.com/aukilabs/hagall-common/http"
	"github.com/aukilabs/hagall-common/messages/hagallpb"
	hwebsocket "github.com/aukilabs/hagall-common/websocket"
	"github.com/aukilabs/hagall/modules"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"golang.org/x/net/websocket"
)

const (
	errTypeLabel        = "error_type"
	msgTypeLabel        = "msg_type"
	moduleLabel         = "module"
	publicEndpointLabel = "public_endpoint"
	appKeyLabel         = "app_key"

	defaultModule = "hagall"
)

var (
	wsConnectedClients = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ws_connected_clients",
		Help: "The number of connected clients.",
	}, []string{
		publicEndpointLabel,
		appKeyLabel,
	})

	wsReceivedMsgs = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ws_received_msgs",
		Help: "The number of messages received from WebSocket connections.",
	}, []string{
		publicEndpointLabel,
		msgTypeLabel,
		appKeyLabel,
	})

	wsReceivedBytes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ws_received_bytes",
		Help: "The number of bytes received from WebSocket connections.",
	}, []string{
		publicEndpointLabel,
		msgTypeLabel,
		appKeyLabel,
	})

	wsReceiveError = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ws_receive_errors",
		Help: "The errors that occured while receiving a websocket message.",
	}, []string{
		publicEndpointLabel,
		errTypeLabel,
		appKeyLabel,
	})

	wsSentMsgs = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ws_sent_msgs",
		Help: "The number of messages sent to WebSocket connections.",
	}, []string{
		publicEndpointLabel,
		msgTypeLabel,
		appKeyLabel,
	})

	wsSentBytes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ws_sent_bytes",
		Help: "The number of bytes sent to WebSocket connections.",
	}, []string{
		publicEndpointLabel,
		msgTypeLabel,
		appKeyLabel,
	})

	wsSendError = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ws_send_errors",
		Help: "The errors that occured while sending a websocket message.",
	}, []string{
		publicEndpointLabel,
		errTypeLabel,
		msgTypeLabel,
		appKeyLabel,
	})

	wsMsgLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "ws_msg_latency",
		Help: "The time to process a WebSocket msg.",
	}, []string{
		publicEndpointLabel,
		msgTypeLabel,
		moduleLabel,
	})
)

func HandlerWithMetrics(h Handler, publicEndpoint string) Handler {
	return &handlerWithMetrics{
		Handler:        h,
		publicEndpoint: publicEndpoint,
	}
}

type handlerWithMetrics struct {
	Handler

	appKey         string
	publicEndpoint string
}

func (h *handlerWithMetrics) HandleConnect(conn *websocket.Conn) {
	req := conn.Request()
	h.appKey = httpcmn.GetAppKeyFromHagallUserToken(httpcmn.GetUserTokenFromHTTPRequest(req))

	wsConnectedClients.
		With(prometheus.Labels{
			publicEndpointLabel: h.publicEndpoint,
			appKeyLabel:         h.appKey,
		}).
		Inc()

	h.Handler.HandleConnect(conn)
}

func (h *handlerWithMetrics) HandlePing(ctx context.Context, sender hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	return h.measureLatency(msg, defaultModule, func() error {
		return h.Handler.HandlePing(ctx, sender, msg)
	})
}

func (h *handlerWithMetrics) HandleSignedPing(ctx context.Context, sender hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	return h.measureLatency(msg, defaultModule, func() error {
		return h.Handler.HandleSignedPing(ctx, sender, msg)
	})
}

func (h *handlerWithMetrics) HandleParticipantJoin(ctx context.Context, handleFrame func(), sender hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	return h.measureLatency(msg, defaultModule, func() error {
		return h.Handler.HandleParticipantJoin(ctx, handleFrame, sender, msg)
	})
}

func (h *handlerWithMetrics) HandleDisconnect(err error) {
	wsConnectedClients.
		With(prometheus.Labels{
			publicEndpointLabel: h.publicEndpoint,
			appKeyLabel:         h.appKey,
		}).
		Dec()

	h.Handler.HandleDisconnect(err)
}

func (h *handlerWithMetrics) HandleEntityAdd(ctx context.Context, sender hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	return h.measureLatency(msg, defaultModule, func() error {
		return h.Handler.HandleEntityAdd(ctx, sender, msg)
	})
}

func (h *handlerWithMetrics) HandleEntityDelete(ctx context.Context, sender hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	return h.measureLatency(msg, defaultModule, func() error {
		return h.Handler.HandleEntityDelete(ctx, sender, msg)
	})
}

func (h *handlerWithMetrics) HandleEntityUpdatePose(ctx context.Context, msg hwebsocket.Msg) error {
	return h.measureLatency(msg, defaultModule, func() error {
		return h.Handler.HandleEntityUpdatePose(ctx, msg)
	})
}

func (h *handlerWithMetrics) HandleWithModule(ctx context.Context, module modules.Module, sender hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	return h.measureLatency(msg, module.Name(), func() error {
		return h.Handler.HandleWithModule(ctx, module, sender, msg)
	})
}

func (h *handlerWithMetrics) SendSyncClock(ctx context.Context, sender hwebsocket.ResponseSender) error {
	return h.measureLatency(hwebsocket.Msg{Type: hagallpb.MsgType_MSG_TYPE_SYNC_CLOCK}, defaultModule, func() error {
		return h.Handler.SendSyncClock(ctx, sender)
	})
}

func (h *handlerWithMetrics) Receiver() hwebsocket.Receiver {
	receive := h.Handler.Receiver()

	return func() (hwebsocket.Msg, int, error) {
		msg, n, err := receive()
		if err != nil {
			wsReceiveError.
				With(prometheus.Labels{
					publicEndpointLabel: h.publicEndpoint,
					errTypeLabel:        errors.Type(err),
					appKeyLabel:         h.appKey,
				}).
				Inc()
		} else {
			wsReceivedMsgs.
				With(prometheus.Labels{
					publicEndpointLabel: h.publicEndpoint,
					msgTypeLabel:        msg.TypeString(),
					appKeyLabel:         h.appKey,
				}).
				Inc()
		}

		if n != 0 {
			wsReceivedBytes.
				With(prometheus.Labels{
					publicEndpointLabel: h.publicEndpoint,
					msgTypeLabel:        msg.TypeString(),
					appKeyLabel:         h.appKey,
				}).
				Add(float64(n))
		}

		return msg, n, err
	}
}

func (h *handlerWithMetrics) Sender() hwebsocket.Sender {
	sender := h.Handler.Sender()

	return func(msg hwebsocket.Msg) (int, error) {
		msgType := msg.TypeString()

		n, err := sender(msg)
		if err != nil {
			wsSendError.
				With(prometheus.Labels{
					publicEndpointLabel: h.publicEndpoint,
					msgTypeLabel:        msgType,
					errTypeLabel:        errors.Type(err),
					appKeyLabel:         h.appKey,
				}).
				Inc()
		}

		if n != 0 {
			wsSentMsgs.
				With(prometheus.Labels{
					publicEndpointLabel: h.publicEndpoint,
					msgTypeLabel:        msgType,
					appKeyLabel:         h.appKey,
				}).
				Inc()
			wsSentBytes.
				With(prometheus.Labels{
					publicEndpointLabel: h.publicEndpoint,
					msgTypeLabel:        msgType,
					appKeyLabel:         h.appKey,
				}).
				Add(float64(n))
		}

		return n, err
	}
}

func (h *handlerWithMetrics) measureLatency(msg hwebsocket.Msg, module string, f func() error) error {
	start := time.Now()

	err := f()
	if errors.IsType(err, hwebsocket.ErrTypeMsgSkip) {
		return err
	}

	wsMsgLatency.With(prometheus.Labels{
		publicEndpointLabel: h.publicEndpoint,
		msgTypeLabel:        msg.TypeString(),
		moduleLabel:         module,
	}).Observe(time.Since(start).Seconds())

	return err
}
