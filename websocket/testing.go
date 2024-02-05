package websocket

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aukilabs/hagall-common/errors"
	httpcmn "github.com/aukilabs/hagall-common/http"
	"github.com/aukilabs/hagall-common/logs"
	hwebsocket "github.com/aukilabs/hagall-common/websocket"
	"github.com/aukilabs/hagall/models"
	"github.com/aukilabs/hagall/modules"
	"github.com/google/uuid"
	"github.com/segmentio/encoding/json"
	"golang.org/x/net/websocket"
)

// Creates a testing environement to unit test handlers and modules.
func NewTestingEnv(t *testing.T, newHandler func() Handler) (*websocket.Conn, *websocket.Conn, func()) {
	var mutex sync.Mutex
	logger := t.Log

	logs.Encoder = func(v any) ([]byte, error) {
		return json.MarshalIndent(v, "", "  ")
	}

	logs.SetLogger(func(e logs.Entry) {
		mutex.Lock()
		defer mutex.Unlock()

		if logger != nil {
			logger(e)
		}
	})

	errors.Encoder = json.Marshal

	clientA, clientB, close := newTestingEnv(t, newHandler)
	return clientA, clientB, func() {
		mutex.Lock()
		defer mutex.Unlock()
		logger = nil
		close()
	}
}

func newTestingEnv(t *testing.T, newHandler func() Handler) (*websocket.Conn, *websocket.Conn, func()) {
	server := httptest.NewServer(websocket.Server{
		Handshake: func(c *websocket.Config, r *http.Request) error {
			return nil
		},
		Handler: func(conn *websocket.Conn) {
			defer conn.Close()

			handler := newHandler()
			defer handler.Close()

			Handle(context.Background(), conn, handler)
		},
	})

	newConn := func() *websocket.Conn {
		config, err := websocket.NewConfig(
			strings.ReplaceAll(server.URL, "http://", "ws://"),
			"http://localhost",
		)
		if err != nil {
			t.Fatalf("error initializing web socket: %s", err)
		}

		config.Header.Set("User-Agent", "ted")
		config.Header.Set("X-Forwarded-for", "192.0.0.0")
		config.Header.Set(httpcmn.HeaderPosemeshClientID, uuid.NewString())

		conn, err := websocket.DialConfig(config)
		if err != nil {
			t.Fatalf("error dialing web socket: %s", err)
		}

		return conn
	}

	clientA := newConn()
	clientB := newConn()

	return clientA, clientB, func() {
		clientA.Close()
		clientB.Close()
		server.Close()
	}
}

type testClient struct{}

func (s testClient) ServerID() string {
	return "ted"
}

type TestResponseSender struct {
	send    func(hwebsocket.ProtoMsg)
	sendMsg func(hwebsocket.Msg)
}

func newTestHandler(newModule ...func() modules.Module) func() Handler {
	sessionStore := &models.SessionStore{
		DiscoveryService: &testClient{},
	}
	return func() Handler {

		modules := make([]modules.Module, len(newModule))
		for i, nm := range newModule {
			modules[i] = nm()
		}
		var h Handler = &RealtimeHandler{
			ClientSyncClockInterval: time.Millisecond * 250,
			ClientIdleTimeout:       time.Minute,
			FrameDuration:           time.Millisecond * 50,
			Sessions:                sessionStore,
			Modules:                 modules,
		}

		h = HandlerWithLogs(h, time.Millisecond*100)
		h = HandlerWithMetrics(h, "https://auki-test.com")
		return h
	}
}
