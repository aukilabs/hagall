package http

import (
	"context"
	"net/http"
	"sync"

	"github.com/aukilabs/go-tooling/pkg/errors"
	"github.com/aukilabs/go-tooling/pkg/logs"
)

func ListenAndServe(ctx context.Context, servers ...*http.Server) {
	go func() {
		<-ctx.Done()

		for _, s := range servers {
			if err := s.Shutdown(context.Background()); err != nil {
				logs.Warn(errors.Newf("shutting down the server failed").
					WithTag("addr", s.Addr).
					Wrap(err))
			}
		}
	}()

	var wg sync.WaitGroup

	for _, s := range servers {
		wg.Add(1)

		go func(s *http.Server) {
			defer wg.Done()

			logs.WithTag("addr", s.Addr).Info("starting server")

			switch err := s.ListenAndServe(); err {
			case nil, http.ErrServerClosed, context.Canceled:
				logs.WithTag("addr", s.Addr).Info("stopping server")

			default:
				logs.Warn(errors.Newf("server stopped").
					WithTag("addr", s.Addr).
					Wrap(err))
			}
		}(s)
	}

	wg.Wait()
}

// MetricsPathFormatter returns empty string on HTTP 301, 400, 404 or 405 statusCode
func MetricsPathFormatter(statusCode int, path string) string {
	if statusCode == http.StatusMovedPermanently ||
		statusCode == http.StatusBadRequest ||
		statusCode == http.StatusNotFound ||
		statusCode == http.StatusMethodNotAllowed {
		return ""
	}

	return path
}
