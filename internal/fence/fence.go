// Package fence tells a FreeSWITCH pod's drain whether the operator has failed the pod over, so that its calls
// belong to the replacement now.
//
// A member whose node is lost is failed over by deleting its pod at once, without waiting for the kubelet, which
// can't be reached. If the node comes back, its kubelet finds the pod gone and stops it, giving the drain one
// second. FreeSWITCH stopping cleanly would then hang up every call it held with a BYE, and the far end would end
// the call its replacement had recovered. So the drain asks first, and when the pod was failed over it stops
// FreeSWITCH without hanging up (fsctl crash).
//
// The question needs no RBAC: the pod's service account token is bound to the pod, and the API server refuses it
// (401) once the pod is gone, while it accepts it through a graceful delete, which leaves the pod in place until
// it has stopped. The API server keeps a token it accepted as good for about ten seconds, but the pod uses its
// token for nothing else, so this is the first look at it since before the node was lost.
package fence

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
)

// Verdict is what the API server says of the pod's token.
type Verdict int

const (
	// Unknown means the API server couldn't be asked, or answered something else: the drain carries on as usual.
	Unknown Verdict = iota
	// Live means the API server took the token: the pod is still there, being deleted gracefully if at all.
	Live
	// FailedOver means the API server refused the token: the pod it is bound to is gone.
	FailedOver
)

// APIServer returns the API server's in-cluster URL, from the environment the kubelet gives every container.
func APIServer() (string, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return "", fmt.Errorf("KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT are not set")
	}
	return "https://" + net.JoinHostPort(host, port), nil
}

// Client returns an HTTP client that trusts only the cluster's CA, in caFile.
func Client(caFile string) (*http.Client, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificate in %s", caFile)
	}
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}}, nil
}

// Check asks the API server at server whether it takes token, by asking for its API versions (open to anyone
// signed in), and returns what that says of the pod. ctx bounds the request.
func Check(ctx context.Context, c *http.Client, server, token string) (Verdict, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return Unknown, fmt.Errorf("no token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(server, "/")+"/api", nil)
	if err != nil {
		return Unknown, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.Do(req)
	if err != nil {
		return Unknown, err
	}
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return FailedOver, nil
	// Signed in: forbidden still means the token was taken.
	case resp.StatusCode < 300, resp.StatusCode == http.StatusForbidden:
		return Live, nil
	}
	return Unknown, fmt.Errorf("API server answered %s", resp.Status)
}
