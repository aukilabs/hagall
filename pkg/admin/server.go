package admin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/net/netutil"
)

const (
	maxListenerConnections = 64
	maxHeaderBytes         = 8 << 10
)

type Options struct {
	Context        context.Context
	AdminAddress   string
	MetricsAddress string
	State          *State
	Registry       *prometheus.Registry
	RelayInfo      RelayInfoFunc
	Drain          DrainFunc
}

type Servers struct {
	state *State

	adminListener   net.Listener
	metricsListener net.Listener
	adminServer     *http.Server
	metricsServer   *http.Server
	errors          chan error
	closeMu         sync.Mutex
	closed          bool
}

func Start(options Options) (*Servers, error) {
	adminHandler, err := AdminHandler(options)
	if err != nil {
		return nil, err
	}
	metricsHandler, err := MetricsHandler(options.Registry)
	if err != nil {
		return nil, err
	}
	if options.AdminAddress == "" || options.MetricsAddress == "" {
		return nil, errors.New("admin and metrics listen addresses are required")
	}
	adminListener, err := net.Listen("tcp", options.AdminAddress)
	if err != nil {
		return nil, fmt.Errorf("listen on relay admin address: %w", err)
	}
	adminListener = netutil.LimitListener(adminListener, maxListenerConnections)
	metricsListener, err := net.Listen("tcp", options.MetricsAddress)
	if err != nil {
		_ = adminListener.Close()
		return nil, fmt.Errorf("listen on relay metrics address: %w", err)
	}
	metricsListener = netutil.LimitListener(metricsListener, maxListenerConnections)

	servers := &Servers{
		state:           options.State,
		adminListener:   adminListener,
		metricsListener: metricsListener,
		errors:          make(chan error, 2),
		adminServer: &http.Server{
			Handler:           adminHandler,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      (maxDrainDeadlineSeconds + 5) * time.Second,
			IdleTimeout:       30 * time.Second,
			MaxHeaderBytes:    maxHeaderBytes,
		},
		metricsServer: &http.Server{
			Handler:           metricsHandler,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       30 * time.Second,
			MaxHeaderBytes:    maxHeaderBytes,
		},
	}
	go servers.serve("admin", servers.adminServer, adminListener)
	go servers.serve("metrics", servers.metricsServer, metricsListener)
	return servers, nil
}

func (s *Servers) Errors() <-chan error {
	if s == nil {
		return nil
	}
	return s.errors
}

func (s *Servers) AdminAddress() string {
	if s == nil || s.adminListener == nil {
		return ""
	}
	return s.adminListener.Addr().String()
}

func (s *Servers) MetricsAddress() string {
	if s == nil || s.metricsListener == nil {
		return ""
	}
	return s.metricsListener.Addr().String()
}

func (s *Servers) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return nil
	}
	s.state.SetAlive(false)
	joined := errors.Join(s.adminServer.Shutdown(ctx), s.metricsServer.Shutdown(ctx))
	if joined == nil {
		s.closed = true
	}
	return joined
}

func (s *Servers) serve(name string, server *http.Server, listener net.Listener) {
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		s.errors <- fmt.Errorf("relay %s server: %w", name, err)
	}
}
