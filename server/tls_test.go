package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voxpupuli/webhook-go/config"
	"github.com/voxpupuli/webhook-go/lib/jobs/fakeengine"
)

func TestMain(m *testing.M) {
	fakeengine.MaybeRun() // becomes the fake r10k when the fixture env var is set
	os.Exit(m.Run())
}

// testCA is a throwaway certificate authority standing in for a Puppet CA.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

var serial atomic.Int64

func newTestCA(t *testing.T, name string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(serial.Add(1)),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue signs a leaf the way the Puppet CA does: CN = certname, and both
// serverAuth and clientAuth extended key usages.
func (ca *testCA) issue(t *testing.T, cn string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial.Add(1)),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
}

func writeFile(t *testing.T, dir, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// mtlsFixture is a CA, a server certificate and the config that uses them.
type mtlsFixture struct {
	ca   *testCA
	dir  string
	conf config.Config
}

func newMTLSFixture(t *testing.T, allowed ...string) *mtlsFixture {
	t.Helper()
	dir := t.TempDir()
	ca := newTestCA(t, "Puppet CA: test")
	sc, sk := ca.issue(t, "puppet.example.test")
	var c config.Config
	c.Server.TLS.Certificate = writeFile(t, dir, "server.pem", sc)
	c.Server.TLS.Key = writeFile(t, dir, "server.key", sk)
	c.Server.TLS.ClientCA = writeFile(t, dir, "ca.pem", ca.pem)
	c.Server.TLS.AllowedCertnames = allowed
	c.Deploys.Basedir = "/etc/puppetlabs/code/environments"
	c.Deploys.Timeout = time.Minute
	c.Deploys.OutputLimit = 262144
	return &mtlsFixture{ca: ca, dir: dir, conf: c}
}

// serve starts newMTLSServer on a loopback port and returns its base URL.
func (f *mtlsFixture) serve(t *testing.T, h http.Handler) string {
	t.Helper()
	srv, err := newMTLSServer(f.conf, h)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.ServeTLS(ln, "", "")
	t.Cleanup(func() { srv.Close() })
	return "https://" + ln.Addr().String()
}

// client returns an HTTPS client that trusts the fixture CA and presents the
// given client certificate (nil for none).
func (f *mtlsFixture) client(t *testing.T, cert *tls.Certificate) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(f.ca.pem)
	tc := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if cert != nil {
		tc.Certificates = []tls.Certificate{*cert}
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: tc}}
}

func clientCert(t *testing.T, ca *testCA, cn string) *tls.Certificate {
	t.Helper()
	c, k := ca.issue(t, cn)
	pair, err := tls.X509KeyPair(c, k)
	if err != nil {
		t.Fatal(err)
	}
	return &pair
}

// countingHandler counts how many requests reached a handler.
type countingHandler struct {
	n    atomic.Int64
	next http.Handler
}

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.n.Add(1)
	h.next.ServeHTTP(w, r)
}

func TestMTLSAllowsListedCertname(t *testing.T) {
	f := newMTLSFixture(t, "stagehand-code-manager")
	h := &countingHandler{next: NewRouter()}
	url := f.serve(t, h)

	resp, err := f.client(t, clientCert(t, f.ca, "stagehand-code-manager")).Get(url + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if h.n.Load() != 1 {
		t.Fatalf("handler ran %d times", h.n.Load())
	}
}

func TestMTLSRefusesUnlistedOrForeign(t *testing.T) {
	f := newMTLSFixture(t, "stagehand-code-manager")
	h := &countingHandler{next: NewRouter()}
	url := f.serve(t, h)
	foreign := newTestCA(t, "Some other CA")

	cases := map[string]*tls.Certificate{
		"unlisted certname":     clientCert(t, f.ca, "intruder"),
		"listed CN, foreign CA": clientCert(t, foreign, "stagehand-code-manager"),
		"no client certificate": nil,
	}
	for name, cert := range cases {
		t.Run(name, func(t *testing.T) {
			resp, err := f.client(t, cert).Get(url + "/health")
			if err == nil {
				resp.Body.Close()
				t.Fatalf("request succeeded with status %d", resp.StatusCode)
			}
			if !strings.Contains(err.Error(), "tls") && !strings.Contains(err.Error(), "EOF") &&
				!strings.Contains(err.Error(), "reset") {
				t.Fatalf("unexpected error kind: %v", err)
			}
		})
	}
	if h.n.Load() != 0 {
		t.Fatalf("handler ran %d times, want 0", h.n.Load())
	}
}

func TestNewMTLSServerRejectsEmptyCAFile(t *testing.T) {
	f := newMTLSFixture(t, "stagehand-code-manager")
	f.conf.Server.TLS.ClientCA = writeFile(t, f.dir, "empty.pem", []byte("not a certificate\n"))
	if _, err := newMTLSServer(f.conf, NewRouter()); err == nil || !strings.Contains(err.Error(), "server.tls.client_ca") {
		t.Fatalf("err = %v, want one naming server.tls.client_ca", err)
	}
}

// freePort returns a loopback port nothing is listening on.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// assertNotListening proves a refused start never opened its listener.
func assertNotListening(t *testing.T, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		t.Fatalf("port %d is in use after a refused start: %v", port, err)
	}
	ln.Close()
}

func TestStartRefusesClientCAWithoutAllowList(t *testing.T) {
	for name, allowed := range map[string][]string{"empty list": nil, "blank entry": {" "}} {
		t.Run(name, func(t *testing.T) {
			f := newMTLSFixture(t, allowed...)
			f.conf.Server.Port = freePort(t)
			srv, err := newMTLSServer(f.conf, NewRouter())
			if err == nil || srv != nil || !strings.Contains(err.Error(), "server.tls.allowed_certnames") {
				t.Fatalf("newMTLSServer = %v, %v; want an error naming server.tls.allowed_certnames", srv, err)
			}
			assertNotListening(t, f.conf.Server.Port)
		})
	}
}

func TestStartRefusesClientCAWithoutServerCert(t *testing.T) {
	for name, clear := range map[string]func(*config.Config){
		"no certificate": func(c *config.Config) { c.Server.TLS.Certificate = "" },
		"no key":         func(c *config.Config) { c.Server.TLS.Key = "" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newMTLSFixture(t, "stagehand-code-manager")
			f.conf.Server.Port = freePort(t)
			clear(&f.conf)
			srv, err := newMTLSServer(f.conf, NewRouter())
			if err == nil || srv != nil ||
				!strings.Contains(err.Error(), "server.tls.certificate and server.tls.key are required when server.tls.client_ca is set") {
				t.Fatalf("newMTLSServer = %v, %v; want the missing server certificate named", srv, err)
			}
			assertNotListening(t, f.conf.Server.Port)
		})
	}
}

func TestLegacyRoutesAbsentInMTLSMode(t *testing.T) {
	payload := func(t *testing.T) *os.File {
		p, err := os.Open("../lib/parsers/json/github/push_not_deleted.json")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { p.Close() })
		return p
	}

	t.Run("mTLS on", func(t *testing.T) {
		f := newMTLSFixture(t, "stagehand-code-manager")
		loadMTLSConfig(t, f, "")
		url := f.serve(t, NewRouter())
		req, _ := http.NewRequest(http.MethodPost, url+"/api/v1/r10k/environment", payload(t))
		req.Header.Set("X-GitHub-Event", "push")
		resp, err := f.client(t, clientCert(t, f.ca, "stagehand-code-manager")).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("legacy route status = %d in mTLS mode, want 404", resp.StatusCode)
		}
	})

	t.Run("mTLS off", func(t *testing.T) {
		cfg := "../lib/helpers/yaml/webhook.queue.yaml"
		config.Init(&cfg)
		router := NewRouter()
		// No provider header: upstream's handler is reached and rejects the
		// body, so no engine runs, but the route exists (not 404).
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/r10k/environment", payload(t))
		router.ServeHTTP(w, req)
		if w.Code == http.StatusNotFound {
			t.Fatalf("legacy route answered 404 with mTLS off; upstream routing must be unchanged")
		}
		w = httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/deploys", nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("deploy API answered %d with mTLS off, want 404", w.Code)
		}
	})
}

// Go's crypto/tls skips VerifyPeerCertificate when a session is resumed, so a
// check placed there would let a resumed session through. Two servers share
// session-ticket keys; the client gets a ticket from the first, then resumes
// against the second. The positive control proves resumption really happens;
// the negative case proves the allow-list is enforced on a resumed session.
func TestMTLSAllowListEnforcedOnResumedSession(t *testing.T) {
	f := newMTLSFixture(t, "stagehand-code-manager")
	cert := clientCert(t, f.ca, "stagehand-code-manager")
	var ticketKey [32]byte
	if _, err := rand.Read(ticketKey[:]); err != nil {
		t.Fatal(err)
	}

	serveWith := func(t *testing.T, allowed ...string) string {
		conf := f.conf
		conf.Server.TLS.AllowedCertnames = allowed
		srv, err := newMTLSServer(conf, NewRouter())
		if err != nil {
			t.Fatal(err)
		}
		srv.TLSConfig.SetSessionTicketKeys([][32]byte{ticketKey})
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go srv.ServeTLS(ln, "", "")
		t.Cleanup(func() { srv.Close() })
		return "https://" + ln.Addr().String()
	}

	newClient := func(cache tls.ClientSessionCache) *http.Client {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(f.ca.pem)
		return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
			DisableKeepAlives: true,
			TLSClientConfig: &tls.Config{
				RootCAs: pool, MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{*cert},
				ServerName:         "localhost", // one session-cache key across both servers
				ClientSessionCache: cache,
			},
		}}
	}

	get := func(c *http.Client, url string) (*http.Response, error) {
		resp, err := c.Get(url + "/health")
		if err == nil {
			io.Copy(io.Discard, resp.Body) // reading lets the client store the TLS 1.3 ticket
			resp.Body.Close()
		}
		return resp, err
	}

	t.Run("control: a resumed session is accepted when listed", func(t *testing.T) {
		cache := tls.NewLRUClientSessionCache(4)
		c := newClient(cache)
		if _, err := get(c, serveWith(t, "stagehand-code-manager")); err != nil {
			t.Fatal(err)
		}
		resp, err := get(c, serveWith(t, "stagehand-code-manager"))
		if err != nil {
			t.Fatal(err)
		}
		if !resp.TLS.DidResume {
			t.Fatal("second connection did not resume; the negative case would prove nothing")
		}
	})

	t.Run("a resumed session is refused when no longer listed", func(t *testing.T) {
		cache := tls.NewLRUClientSessionCache(4)
		c := newClient(cache)
		if _, err := get(c, serveWith(t, "stagehand-code-manager")); err != nil {
			t.Fatal(err)
		}
		resp, err := get(c, serveWith(t, "someone-else"))
		if err == nil {
			t.Fatalf("resumed session accepted (DidResume=%v, status %d)", resp.TLS.DidResume, resp.StatusCode)
		}
	})
}
