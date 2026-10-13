package server

// Stagehand: the mTLS-only server (D-10). When server.tls.client_ca is set the
// whole server, /health included, requires a client certificate that chains
// to that CA (the estate's Puppet CA) and whose Subject CN, the Puppet
// certname, is listed in server.tls.allowed_certnames. Both checks happen in
// the TLS handshake, before any handler runs. The CRL and an optional
// fingerprint pin follow in plan 57.6-06.

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/voxpupuli/webhook-go/config"
)

var errCertnameNotAllowed = errors.New("client certificate certname is not in server.tls.allowed_certnames")

// newMTLSServer returns an http.Server for handler that enforces mutual TLS.
// It loads and checks every file it needs, so a bad configuration fails here
// instead of after the listener opens. Serve it with ListenAndServeTLS("", "").
func newMTLSServer(cfg config.Config, handler http.Handler) (*http.Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	t := cfg.Server.TLS
	if t.ClientCA == "" {
		return nil, errors.New("server.tls.client_ca is not set")
	}

	caPEM, err := os.ReadFile(t.ClientCA)
	if err != nil {
		return nil, fmt.Errorf("server.tls.client_ca: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("server.tls.client_ca: no PEM certificates found in %s", t.ClientCA)
	}

	pair, err := tls.LoadX509KeyPair(t.Certificate, t.Key)
	if err != nil {
		return nil, fmt.Errorf("server.tls.certificate and server.tls.key: %w", err)
	}

	allowed := make(map[string]bool, len(t.AllowedCertnames))
	for _, name := range t.AllowedCertnames {
		allowed[name] = true
	}

	return &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{pair},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    pool,
			// VerifyConnection, not VerifyPeerCertificate: crypto/tls skips
			// VerifyPeerCertificate when a session is resumed, but calls
			// VerifyConnection on every handshake, resumed ones included, with
			// the chains verified against ClientCAs when the session began.
			// Every per-certificate check (the allow-list here; the CRL and
			// fingerprint pin of plan 57.6-06) must live in this callback, or a
			// resumed session skips it (TestMTLSAllowListEnforcedOnResumedSession).
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.VerifiedChains) == 0 || len(cs.VerifiedChains[0]) == 0 {
					return errCertnameNotAllowed
				}
				cn := cs.VerifiedChains[0][0].Subject.CommonName
				if !allowed[cn] {
					// The certname only; never the certificate.
					log.Warnf("mTLS: refused client certname %q: not in server.tls.allowed_certnames", cn)
					return errCertnameNotAllowed
				}
				return nil
			},
		},
	}, nil
}
