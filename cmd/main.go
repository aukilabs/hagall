package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aukilabs/hagall/pkg/admin"
	"github.com/aukilabs/hagall/pkg/app"
	"github.com/aukilabs/hagall/pkg/config"
)

var version = "v0.0.0"

type relayApplication interface {
	Wait() error
	DrainForSignal(context.Context, admin.ShutdownIntent, time.Duration) (admin.DrainResult, error)
	Close(context.Context) error
}

var startRelayApplication = func(ctx context.Context, cfg config.Config, buildVersion string, logger *slog.Logger) (relayApplication, error) {
	return app.Start(ctx, cfg, buildVersion, logger)
}

type relayApplicationStartResult struct {
	application relayApplication
	err         error
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load()
	if err != nil {
		logger.Error("invalid standalone relay configuration", "error", err)
		os.Exit(1)
	}
	redacted, err := cfg.MarshalRedactedJSON()
	if err != nil {
		logger.Error("encode redacted standalone relay configuration", "error", err)
		os.Exit(1)
	}
	logger.Info("starting standalone relay Node", "version", version, "config", string(redacted))

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	if err := run(context.Background(), signals, cfg, version, logger); err != nil {
		logger.Error("standalone relay Node stopped with an error", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, signals <-chan os.Signal, cfg config.Config, buildVersion string, logger *slog.Logger) error {
	applicationContext, cancelApplication := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelApplication()
	startResult := make(chan relayApplicationStartResult)
	// If startup ignores cancellation past the bounded wait below, ownership is
	// handed back to this goroutine. That guarantees any application handle that
	// eventually emerges is drained and closed instead of being orphaned.
	abandonedStartup := make(chan admin.ShutdownIntent, 1)
	go func() {
		application, err := startRelayApplication(applicationContext, cfg, buildVersion, logger)
		result := relayApplicationStartResult{application: application, err: err}
		select {
		case startResult <- result:
		case intent := <-abandonedStartup:
			cleanupErr := errors.Join(result.err, drainAndClose(application, intent, cfg))
			if cleanupErr != nil {
				logger.Error("clean up relay application after startup cancellation deadline", "error", cleanupErr)
			}
		}
	}()

	var started relayApplicationStartResult
	var startupShutdownIntent admin.ShutdownIntent
	startupInterrupted := false
	select {
	case started = <-startResult:
	case signalValue := <-signals:
		startupShutdownIntent = shutdownIntentForSignal(signalValue)
		startupInterrupted = true
	case <-ctx.Done():
		startupShutdownIntent = admin.ShutdownIntentRestartSameIdentity
		startupInterrupted = true
	}

	if startupInterrupted {
		logger.Info("interrupting standalone relay Node startup", "shutdown_intent", startupShutdownIntent)
		cancelApplication()
		startupTimeout := 2 * cfg.HTTP.RequestTimeout
		if startupTimeout <= 0 {
			startupTimeout = time.Second
		}
		timer := time.NewTimer(startupTimeout)
		defer timer.Stop()
		select {
		case started = <-startResult:
			return errors.Join(started.err, drainAndClose(started.application, startupShutdownIntent, cfg))
		case <-timer.C:
			// The startup goroutine retains cleanup ownership and will consume
			// this intent before it can exit if Start returns later.
			abandonedStartup <- startupShutdownIntent
			return fmt.Errorf("relay application startup did not stop within %s after cancellation: %w", startupTimeout, context.DeadlineExceeded)
		}
	}

	if started.err != nil {
		return errors.Join(started.err, closeRelayApplication(started.application, cfg))
	}
	if started.application == nil {
		return errors.New("relay application startup returned no application")
	}
	relayApplication := started.application
	waitResult := make(chan error, 1)
	go func() { waitResult <- relayApplication.Wait() }()

	var runErr error
	select {
	case runErr = <-waitResult:
	case signalValue := <-signals:
		intent := shutdownIntentForSignal(signalValue)
		logger.Info("draining standalone relay Node", "shutdown_intent", intent)
		drainContext, cancelDrain := context.WithTimeout(context.Background(), cfg.ShutdownDrain+cfg.HTTP.RequestTimeout)
		_, runErr = relayApplication.DrainForSignal(drainContext, intent, cfg.ShutdownDrain)
		cancelDrain()
	case <-ctx.Done():
		drainContext, cancelDrain := context.WithTimeout(context.Background(), cfg.ShutdownDrain+cfg.HTTP.RequestTimeout)
		_, runErr = relayApplication.DrainForSignal(drainContext, admin.ShutdownIntentRestartSameIdentity, cfg.ShutdownDrain)
		cancelDrain()
	}
	return errors.Join(runErr, closeRelayApplication(relayApplication, cfg))
}

func drainAndClose(application relayApplication, intent admin.ShutdownIntent, cfg config.Config) error {
	if application == nil {
		return nil
	}
	drainContext, cancelDrain := context.WithTimeout(context.Background(), cfg.ShutdownDrain+cfg.HTTP.RequestTimeout)
	_, drainErr := application.DrainForSignal(drainContext, intent, cfg.ShutdownDrain)
	cancelDrain()
	return errors.Join(drainErr, closeRelayApplication(application, cfg))
}

func closeRelayApplication(application relayApplication, cfg config.Config) error {
	if application == nil {
		return nil
	}
	shutdownTimeout := cfg.HTTP.RequestTimeout + 10*time.Second
	shutdownContext, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return application.Close(shutdownContext)
}

func shutdownIntentForSignal(value os.Signal) admin.ShutdownIntent {
	if value == os.Interrupt {
		return admin.ShutdownIntentReassign
	}
	return admin.ShutdownIntentRestartSameIdentity
}
