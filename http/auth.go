package http

import (
	"context"
	"net/http"

	hds "github.com/aukilabs/hagall-common/hdsclient"
	httpcmn "github.com/aukilabs/hagall-common/http"
	"github.com/aukilabs/hagall-common/logs"
	"golang.org/x/net/websocket"
)

func VerifyAuthToken(ctx context.Context, hdsClient *hds.Client) func(*websocket.Config, *http.Request) error {
	return func(c *websocket.Config, r *http.Request) error {
		token := httpcmn.GetUserTokenFromHTTPRequest(r)

		if err := hdsClient.VerifyUserAuth(token); err != nil {
			logs.WithClientID(r.Header.Get(httpcmn.HeaderPosemeshClientID)).Error(err)
			return err
		}

		return nil
	}
}

func VerifyAuthTokenHandler(hdsClient *hds.Client, next http.HandlerFunc) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		token := httpcmn.GetUserTokenFromHTTPRequest(r)

		if err := hdsClient.VerifyUserAuth(token); err != nil {
			logs.WithClientID(r.Header.Get(httpcmn.HeaderPosemeshClientID)).Error(err)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	}
}
