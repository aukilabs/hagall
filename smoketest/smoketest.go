package smoketest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/aukilabs/go-tooling/pkg/errors"
	"github.com/aukilabs/go-tooling/pkg/logs"
	hsmoketest "github.com/aukilabs/hagall-common/smoketest"
)

type Options struct {
	Endpoint              string
	UserAgent             string
	MakeHagallServerToken func(string, string, time.Duration) (string, error)
	SendResult            func(context.Context, hsmoketest.SmokeTestResults) error
}

type testCtxKey string

var testCtxKeyValue testCtxKey = "test-context"

type testContext struct {
	context.Context
	Cancel func()
}

func HandleSmokeTest(ctx context.Context, opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			logs.Error(errors.New("reading body failed").Wrap(err))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		var req hsmoketest.SmokeTestRequest
		if err := json.Unmarshal(b, &req); err != nil {
			logs.Error(errors.New("marshaling request failed").Wrap(err))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		go func() {
			defer func() {
				if tctx := ctx.Value(testCtxKeyValue); tctx != nil {
					testCtx := tctx.(testContext)
					if testCtx.Cancel != nil {
						testCtx.Cancel()
					}
				}
			}()
			res, err := hsmoketest.RunSmokeTest(ctx, hsmoketest.RunSmokeTestOptions{
				FromEndpoint:       opts.Endpoint,
				ToEndpoint:         req.Endpoint,
				ToEndpointToken:    req.Token,
				Timeout:            req.Timeout,
				MaxSessionIDLength: req.MaxSessionIDLength,
			})
			if err != nil {
				logs.Error(err)
			}

			if err := opts.SendResult(ctx, res); err != nil {
				logs.WithTag("from_endpoint", opts.Endpoint).
					WithTag("to_endpoint", req.Endpoint).
					Error(errors.New("sending smoke test result failed").Wrap(err))
			}

		}()

		w.WriteHeader(http.StatusOK)
	}
}
