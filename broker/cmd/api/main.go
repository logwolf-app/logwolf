package main

import (
	"context"
	"fmt"
	"log"
	"logwolf-toolbox/event"
	"logwolf-toolbox/limits"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	shutdownTimeout = 30 * time.Second
)

type Config struct {
	// Events publishes to RabbitMQ; see event.Emitter.
	Events publisher

	// TrustedProxies are the peers whose X-Forwarded-For is believed when
	// working out a client's address (see clientIP). Empty trusts no one.
	TrustedProxies []netip.Prefix

	// Limits is what the edition lets a project do (see limitsProvider).
	Limits limits.Provider
}

// limitsProvider is app.Limits, or self-hosted's when there is none: a Config
// left without limits is a self-hosted broker, the way tests build one.
func (app *Config) limitsProvider() limits.Provider {
	if app.Limits == nil {
		return limits.SelfHosted{}
	}
	return app.Limits
}

func main() {
	trusted, err := trustedProxiesFromEnv()
	if err != nil {
		log.Panic(err)
	}

	// LOGWOLF_EDITION, self-hosted unless set otherwise.
	lim, err := limits.FromEnv()
	if err != nil {
		log.Panic(err)
	}

	// RabbitMQ
	emitter, err := event.NewEmitter(rabbitConnectionString())
	if err != nil {
		log.Panic(err)
	}
	defer emitter.Close()

	app := Config{
		Events:         emitter,
		TrustedProxies: trusted,
		Limits:         lim,
	}

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%s", httpPort()),
		Handler: app.routes(),
	}

	// Listen for SIGTERM/SIGINT in the background. When received, gracefully
	// drain in-flight HTTP requests before closing the RabbitMQ connection.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go sweepAuthCachesEvery(ctx, authCacheSweepInterval)

	go func() {
		log.Printf("Starting server on port %s\n", httpPort())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Panicf("ListenAndServe: %v", err)
		}
	}()

	// Block until a signal is received.
	<-ctx.Done()
	log.Println("Shutdown signal received — draining in-flight requests...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP shutdown error: %v", err)
	}

	log.Println("Shutdown complete.")
}

func rabbitConnectionString() string {
	if u := os.Getenv("RABBITMQ_URL"); u != "" {
		return u
	}
	return "amqp://guest:guest@rabbitmq"
}

func httpPort() string {
	if u := os.Getenv("BROKER_PORT"); u != "" {
		return u
	}
	return "80"
}

func loggerRPCAddr() string {
	if a := os.Getenv("LOGGER_RPC_ADDR"); a != "" {
		return a
	}
	return "logger:5001"
}
