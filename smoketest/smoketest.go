package smoketest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/aukilabs/go-tooling/pkg/errors"
	"github.com/aukilabs/go-tooling/pkg/logs"
	httpcmn "github.com/aukilabs/hagall-common/http"
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
			httpcmn.InternalServerError(w, errors.New("reading body failed").Wrap(err))
			return
		}

		var req hsmoketest.SmokeTestRequest
		if err := json.Unmarshal(b, &req); err != nil {
			httpcmn.BadRequest(w, httpcmn.ErrBadRequest)
			return
		}

		go func() {
			defer func() {
				// if context is of testContext
				// cancel context on exit to signal function exited
				// this is used for testing
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
				logs.Warn(err)
			}

			if err := opts.SendResult(ctx, res); err != nil {
				logs.WithTag("from_endpoint", opts.Endpoint).
					WithTag("to_endpoint", req.Endpoint).
					Warn(errors.New("sending smoke test result failed").Wrap(err))
			}

		}()

		w.WriteHeader(http.StatusOK)
	}
}
