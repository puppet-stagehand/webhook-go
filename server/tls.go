package server

// Stagehand: the mTLS-only server (D-10). When server.tls.client_ca is set the
// whole server, /health included, requires a client certificate that chains
// to that CA (the estate's Puppet CA) and whose Subject CN, the Puppet
// certname, is listed in server.tls.allowed_certnames. Optionally the
// certificate must not be revoked by the CA's CRL (server.tls.crl, crl.go) and
// its SHA-256 fingerprint must be pinned (server.tls.allowed_fingerprints).
// Every check happens in the TLS handshake, before any handler runs.

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/voxpupuli/webhook-go/config"
)

var (
	errCertnameNotAllowed = errors.New("client certificate certname is not in server.tls.allowed_certnames")
	errCertRevoked        = errors.New("client certificate is revoked by server.tls.crl")
	errCertNotPinned      = errors.New("client certificate fingerprint is not in server.tls.allowed_fingerprints")
)

// newMTLSServer returns an http.Server for handler that enforces mutual TLS.
// It loads and checks every file it needs, so a bad configuration fails here
// instead of after the listener opens. Serve it with ListenAndServeTLS("", "").
func newMTLSServer(cfg config.Config, handler http.Handler) (*http.Server, error) {
	return newMTLSServerWithClock(cfg, handler, time.Now)
}

// fingerprint is the SHA-256 of a certificate's DER, as Puppet prints it.
func fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	var b strings.Builder
	for i, v := range sum {
		if i > 0 {
			b.WriteByte(':')
		}
		fmt.Fprintf(&b, "%02X", v)
	}
	return b.String()
}

// newMTLSServerWithClock is newMTLSServer with the clock the CRL reload
// interval is measured on (tests inject one).
func newMTLSServerWithClock(cfg config.Config, handler http.Handler, now func() time.Time) (*http.Server, error) {
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

	var crl *crlSet
	if t.CRL != "" {
		if crl, err = loadCRL(t.CRL, caPEM, now); err != nil {
			return nil, fmt.Errorf("server.tls.crl: %w", err)
		}
	}
	pins := make(map[string]bool, len(t.AllowedFingerprints))
	for i, fp := range t.AllowedFingerprints {
		norm, ok := config.NormalizeFingerprint(fp)
		if !ok { // Validate already refused it; kept for direct callers.
			return nil, fmt.Errorf("server.tls.allowed_fingerprints[%d] is not a SHA-256 fingerprint", i)
		}
		pins[norm] = true
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
			// Every per-certificate check (the allow-list, the CRL and the
			// fingerprint pin) must live in this callback, or a resumed
			// session skips it (TestMTLSAllowListEnforcedOnResumedSession).
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.VerifiedChains) == 0 || len(cs.VerifiedChains[0]) == 0 {
					return errCertnameNotAllowed
				}
				chain := cs.VerifiedChains[0]
				leaf := chain[0]
				cn := leaf.Subject.CommonName
				// Log the certname and serial only; never the certificate.
				if !allowed[cn] {
					log.Warnf("mTLS: refused client certname %q: not in server.tls.allowed_certnames", cn)
					return errCertnameNotAllowed
				}
				if crl != nil {
					// The leaf and any intermediate CA; the root has no issuer CRL.
					for _, c := range chain[:max(1, len(chain)-1)] {
						if crl.Revoked(c) {
							log.Warnf("mTLS: refused client certname %q: certificate serial %s is revoked", cn, c.SerialNumber)
							return errCertRevoked
						}
					}
				}
				if len(pins) > 0 && !pins[fingerprint(leaf)] {
					log.Warnf("mTLS: refused client certname %q: fingerprint not in server.tls.allowed_fingerprints", cn)
					return errCertNotPinned
				}
				return nil
			},
		},
	}, nil
}
