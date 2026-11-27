package fence

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A refused token means the pod was failed over; one taken, even without access, means it is still there.
func TestCheck(t *testing.T) {
	cases := []struct {
		status int
		want   Verdict
		err    bool
	}{
		{http.StatusOK, Live, false},
		{http.StatusForbidden, Live, false},
		{http.StatusUnauthorized, FailedOver, false},
		{http.StatusInternalServerError, Unknown, true},
		{http.StatusTooManyRequests, Unknown, true},
	}
	for _, c := range cases {
		t.Run(http.StatusText(c.status), func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api" || r.Header.Get("Authorization") != "Bearer tok" {
					t.Errorf("asked %s with %q", r.URL.Path, r.Header.Get("Authorization"))
				}
				w.WriteHeader(c.status)
			}))
			defer srv.Close()
			client := clientTrusting(t, srv)
			got, err := Check(context.Background(), client, srv.URL+"/", "tok\n")
			if got != c.want || (err != nil) != c.err {
				t.Fatalf("got %v, %v; want %v (error %v)", got, err, c.want, c.err)
			}
		})
	}
}

// A server that doesn't answer in time, or isn't the cluster's, tells nothing.
func TestCheckUnknown(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
	defer srv.Close()
	defer close(block)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if got, err := Check(ctx, clientTrusting(t, srv), srv.URL, "tok"); got != Unknown || err == nil {
		t.Fatalf("slow server: %v, %v", got, err)
	}

	refusing := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer refusing.Close()
	if got, err := Check(context.Background(), clientTrustingOther(t), refusing.URL, "tok"); got != Unknown || err == nil {
		t.Fatalf("untrusted server: %v, %v", got, err)
	}
	if got, err := Check(context.Background(), clientTrusting(t, srv), srv.URL, " "); got != Unknown || err == nil {
		t.Fatalf("no token: %v, %v", got, err)
	}
}

func TestAPIServer(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "fd00::1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	if got, err := APIServer(); err != nil || got != "https://[fd00::1]:443" {
		t.Fatalf("%q, %v", got, err)
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	if _, err := APIServer(); err == nil {
		t.Fatal("no host, no error")
	}
}

// clientTrusting is Client, given a file with srv's certificate.
func clientTrusting(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	return clientTrustingDER(t, srv.Certificate().Raw)
}

// clientTrustingOther is Client, given a CA of its own that signed no test server's certificate.
func clientTrustingOther(t *testing.T) *http.Client {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "other"}, IsCA: true,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return clientTrustingDER(t, der)
}

func clientTrustingDER(t *testing.T, der []byte) *http.Client {
	t.Helper()
	ca := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Client(ca)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
