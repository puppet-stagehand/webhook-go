package server

// Stagehand: certificate revocation for the mTLS server (MH-05, T-57.6-33).
// Go's crypto/tls does not consult CRLs, so `puppetserver ca revoke` would
// otherwise change nothing here. The CRL named by server.tls.crl is loaded at
// start (a CRL that does not parse refuses the start) and reloaded when its
// modification time or size changes, checked at most once a minute. A reload
// that fails keeps the last good list and logs an error, so a half-written or
// deleted file never silently un-revokes anyone.

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// crlReloadInterval is the most often the CRL file is checked for a change.
const crlReloadInterval = time.Minute

// crlSet is the revoked certificates of every CRL in one file, keyed by
// issuer and serial number, so serials from different CAs never collide.
type crlSet struct {
	path    string
	issuers []*x509.Certificate // the CAs from server.tls.client_ca
	now     func() time.Time

	mu      sync.Mutex
	revoked map[string]struct{}
	modTime time.Time
	size    int64
	checked time.Time
}

func revocationKey(rawIssuer []byte, serial fmt.Stringer) string {
	return string(rawIssuer) + "\x00" + serial.String()
}

// loadCRL reads path and checks that every CRL in it is signed by one of the
// CAs in caPEM. now is the clock the reload interval is measured on.
func loadCRL(path string, caPEM []byte, now func() time.Time) (*crlSet, error) {
	var issuers []*x509.Certificate
	for rest := caPEM; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parsing server.tls.client_ca: %w", err)
		}
		issuers = append(issuers, c)
	}
	c := &crlSet{path: path, issuers: issuers, now: now}
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	revoked, err := c.parse(b)
	if err != nil {
		return nil, err
	}
	c.revoked, c.modTime, c.size, c.checked = revoked, st.ModTime(), st.Size(), now()
	return c, nil
}

// parse reads every CRL in b (PEM "X509 CRL" blocks, or one DER CRL).
func (c *crlSet) parse(b []byte) (map[string]struct{}, error) {
	var ders [][]byte
	for rest := b; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type == "X509 CRL" {
			ders = append(ders, block.Bytes)
		}
	}
	if len(ders) == 0 {
		if len(bytes.TrimSpace(b)) == 0 {
			return nil, errors.New("the file is empty")
		}
		ders = [][]byte{b} // perhaps DER
	}
	revoked := map[string]struct{}{}
	for i, der := range ders {
		rl, err := x509.ParseRevocationList(der)
		if err != nil {
			return nil, fmt.Errorf("CRL %d: %w", i+1, err)
		}
		if err := c.checkIssuer(rl); err != nil {
			return nil, fmt.Errorf("CRL %d: %w", i+1, err)
		}
		for _, e := range rl.RevokedCertificateEntries {
			revoked[revocationKey(rl.RawIssuer, e.SerialNumber)] = struct{}{}
		}
	}
	return revoked, nil
}

func (c *crlSet) checkIssuer(rl *x509.RevocationList) error {
	for _, ca := range c.issuers {
		if bytes.Equal(ca.RawSubject, rl.RawIssuer) && rl.CheckSignatureFrom(ca) == nil {
			return nil
		}
	}
	return fmt.Errorf("issued by %q, which is not a CA in server.tls.client_ca that signed it", rl.Issuer.String())
}

// maybeReload re-reads the file when the reload interval has passed and the
// file changed. On any failure the last good list stays in force.
func (c *crlSet) maybeReload() {
	now := c.now()
	if now.Sub(c.checked) < crlReloadInterval {
		return
	}
	c.checked = now
	st, err := os.Stat(c.path)
	if err != nil {
		log.Errorf("server.tls.crl: %v; keeping the last good CRL", err)
		return
	}
	if st.ModTime().Equal(c.modTime) && st.Size() == c.size {
		return
	}
	b, err := os.ReadFile(c.path)
	if err == nil {
		var revoked map[string]struct{}
		if revoked, err = c.parse(b); err == nil {
			c.revoked, c.modTime, c.size = revoked, st.ModTime(), st.Size()
			log.Infof("server.tls.crl: reloaded %s (%d revoked certificates)", c.path, len(revoked))
			return
		}
	}
	// Remember this version so a broken file is reported once, not every
	// minute; a further change is tried again.
	c.modTime, c.size = st.ModTime(), st.Size()
	log.Errorf("server.tls.crl: reloading %s: %v; keeping the last good CRL", c.path, err)
}

// Revoked reports whether cert is listed by the CRL of the CA that issued it.
func (c *crlSet) Revoked(cert *x509.Certificate) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maybeReload()
	_, ok := c.revoked[revocationKey(cert.RawIssuer, cert.SerialNumber)]
	return ok
}
