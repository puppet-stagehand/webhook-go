package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/voxpupuli/webhook-go/config"
	"github.com/voxpupuli/webhook-go/lib/queue"
)

// Init initializes and starts the server on the configured port.
// If queue functionality is enabled in the config, it starts the job queue processing.
// The server can run with or without TLS, depending on the configuration.
func Init() {
	full := config.GetConfig() // Stagehand: newMTLSServer needs the whole config.
	config := config.GetConfig().Server

	if config.Queue.Enabled {
		queue.Work() // Start the job queue if enabled.
	}

	if full.MTLSEnabled() {
		// Stagehand: mTLS-only server (server/tls.go) when server.tls.client_ca is set.
		serveMTLS(full)
		return
	}

	r := NewRouter() // Initialize the router.
	if config.TLS.Enabled {
		// Start the server with TLS (HTTPS) using the provided certificate and key.
		r.RunTLS(":"+fmt.Sprint(config.Port), config.TLS.Certificate, config.TLS.Key)
	} else {
		// Start the server without TLS (HTTP).
		r.Run(":" + fmt.Sprint(config.Port))
	}
}

// shutdownGrace bounds how long a stopping server waits for requests to end
// and for deploy records to be written.
const shutdownGrace = 30 * time.Second

// serveMTLS runs the Stagehand mTLS server until SIGINT or SIGTERM. The signal
// cancels the deploy runner's base context, which stops a running engine (its
// whole process group) and fails queued jobs; their records are written before
// the process exits, so a client polling a job sees it end.
func serveMTLS(full config.Config) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	r, deploys := newRouter(ctx)
	srv, err := newMTLSServer(full, r)
	if err != nil {
		log.Fatalf("refusing to start: %v", err)
	}
	go func() {
		<-ctx.Done()
		log.Info("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			log.Errorf("shutdown: %v", err)
		}
	}()
	err = srv.ListenAndServeTLS("", "")
	if !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	if deploys != nil {
		if err := deploys.Shutdown(shutdownGrace); err != nil {
			log.Error(err)
		}
	}
}
