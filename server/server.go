package server

import (
	"fmt"

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

	r := NewRouter() // Initialize the router.
	if full.MTLSEnabled() {
		// Stagehand: mTLS-only server (server/tls.go) when server.tls.client_ca is set.
		srv, err := newMTLSServer(full, r)
		if err != nil {
			log.Fatalf("refusing to start: %v", err)
		}
		log.Fatal(srv.ListenAndServeTLS("", ""))
	}
	if config.TLS.Enabled {
		// Start the server with TLS (HTTPS) using the provided certificate and key.
		r.RunTLS(":"+fmt.Sprint(config.Port), config.TLS.Certificate, config.TLS.Key)
	} else {
		// Start the server without TLS (HTTP).
		r.Run(":" + fmt.Sprint(config.Port))
	}
}
