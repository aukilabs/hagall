package smoketest

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aukilabs/hagall-common/messages/hagallpb"
	hsmoketest "github.com/aukilabs/hagall-common/smoketest"
	htesting "github.com/aukilabs/hagall-common/testing"
	hwebsocket "github.com/aukilabs/hagall-common/websocket"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestSmokeTest(t *testing.T) {
	t.Run("smoke test success", func(t *testing.T) {
		// prepare
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		// mock hagall messages
		server := htesting.MockHagall(t, ctx, func(conn *websocket.Conn, msg hwebsocket.Msg) {
			switch msg.Type {
			case hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST:
				var req hagallpb.ParticipantJoinRequest
				err := msg.DataTo(&req)
				require.NoError(t, err)
				time.Sleep(1 * time.Millisecond)
				htesting.SendProto(t, conn, &hagallpb.ParticipantJoinResponse{
					Type:          hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE,
					Timestamp:     timestamppb.Now(),
					RequestId:     req.RequestId,
					SessionId:     "testx1",
					ParticipantId: 1,
				})
				htesting.SendProto(t, conn, &hagallpb.SessionState{
					Type:      hagallpb.MsgType_MSG_TYPE_SESSION_STATE,
					Timestamp: timestamppb.Now(),
					Participants: []*hagallpb.Participant{
						{
							Id: 1,
						},
					},
				})
			case hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_REQUEST:
				var req hagallpb.EntityAddRequest
				err := msg.DataTo(&req)
				require.NoError(t, err)
				time.Sleep(1 * time.Millisecond)
				htesting.SendProto(t, conn, &hagallpb.EntityAddResponse{
					Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_RESPONSE,
					Timestamp: timestamppb.Now(),
					RequestId: req.RequestId,
					EntityId:  1,
				})
			}
		})

		ctx = context.WithValue(ctx, testCtxKeyValue, testContext{
			Context: ctx,
			Cancel:  cancel,
		})

		// test
		var gotResult bool
		smokeTest := HandleSmokeTest(ctx, Options{
			Endpoint: "http://localhagall",
			MakeHagallServerToken: func(appKey, secret string, ttl time.Duration) (string, error) {
				return "", nil
			},
			SendResult: func(_ context.Context, res hsmoketest.SmokeTestResults) error {
				require.Equal(t, res.FromEndpoint, "http://localhagall")
				require.Equal(t, res.ToEndpoint, server.URL)
				require.GreaterOrEqual(t, res.LatencyMilliSec, float64(1))
				require.Equal(t, res.Status, hsmoketest.StatusSuccess)
				gotResult = true
				return nil
			},
		})

		stReq := hsmoketest.SmokeTestRequest{
			Endpoint:           server.URL,
			MaxSessionIDLength: 11,
			Timeout:            time.Second,
		}
		body, err := json.Marshal(stReq)
		require.NoError(t, err)

		rdr := bytes.NewBuffer(body)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://localhagall", rdr)

		smokeTest.ServeHTTP(rec, req)

		<-ctx.Done()

		require.True(t, gotResult)
	})

	t.Run("smoke test failed - offline", func(t *testing.T) {
		// prepare
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		ctx = context.WithValue(ctx, testCtxKeyValue, testContext{
			Context: ctx,
			Cancel:  cancel,
		})

		// test
		var gotResult bool
		smokeTest := HandleSmokeTest(ctx, Options{
			Endpoint: "http://localhagall",
			MakeHagallServerToken: func(appKey, secret string, ttl time.Duration) (string, error) {
				return "", nil
			},
			SendResult: func(_ context.Context, res hsmoketest.SmokeTestResults) error {
				require.Equal(t, res.FromEndpoint, "http://localhagall")
				require.Equal(t, res.ToEndpoint, "http://otherhagall")
				require.Equal(t, res.LatencyMilliSec, float64(0))
				require.Equal(t, res.Status, hsmoketest.StatusFailed)
				gotResult = true
				return nil
			},
		})

		stReq := hsmoketest.SmokeTestRequest{
			Endpoint:           "http://otherhagall",
			MaxSessionIDLength: 11,
			Timeout:            time.Second,
		}
		body, err := json.Marshal(stReq)
		require.NoError(t, err)

		rdr := bytes.NewBuffer(body)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://localhagall", rdr)

		smokeTest.ServeHTTP(rec, req)

		<-ctx.Done()

		require.True(t, gotResult)
	})

}
