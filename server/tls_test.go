package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
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
