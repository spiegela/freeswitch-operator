package main

import (
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// Exit 0 makes the drain stop FreeSWITCH without hanging up, so it must mean failed over, and only that. The drain
// reads the numbers themselves, so they are written out here.
func TestCheckExitCode(t *testing.T) {
	cases := []struct {
		status int
		want   int
	}{
		{http.StatusUnauthorized, 0},
		{http.StatusOK, 1},
		{http.StatusForbidden, 1},
		{http.StatusInternalServerError, 2},
	}
	for _, c := range cases {
		t.Run(http.StatusText(c.status), func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
			}))
			defer srv.Close()
			host, port, err := net.SplitHostPort(srv.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("KUBERNETES_SERVICE_HOST", host)
			t.Setenv("KUBERNETES_SERVICE_PORT", port)
			dir := t.TempDir()
			token, ca := filepath.Join(dir, "token"), filepath.Join(dir, "ca.crt")
			writeFile(t, token, []byte("tok"))
			writeFile(t, ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
			if got := check([]string{"-token", token, "-ca", ca}); got != c.want {
				t.Fatalf("exit %d, want %d", got, c.want)
			}
		})
	}
	if got := check([]string{"-token", filepath.Join(t.TempDir(), "none")}); got != 2 {
		t.Fatalf("no token file: exit %d, want 2", got)
	}
}

func writeFile(t *testing.T, name string, b []byte) {
	t.Helper()
	if err := os.WriteFile(name, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
