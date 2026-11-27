// Command freeswitch-fence tells a FreeSWITCH pod's drain whether the operator has failed the pod over (see
// package fence). It ships in the freeswitch-exporter image; in a fleet with spec.recovery an init container copies
// it into the pod, and the drain runs it first.
//
//	freeswitch-fence install DEST   copy this binary to DEST
//	freeswitch-fence check [flags]  exit 0 if the pod was failed over, 1 if it is still there, 2 if that can't be told
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spiegela/freeswitch-operator/internal/fence"
)

const (
	exitFailedOver = 0
	exitLive       = 1
	exitUnknown    = 2
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "install":
		if len(os.Args) != 3 {
			usage()
		}
		if err := install(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, "freeswitch-fence: install:", err)
			os.Exit(1)
		}
	case "check":
		os.Exit(check(os.Args[2:]))
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: freeswitch-fence install DEST | check [-token FILE] [-ca FILE] [-timeout D]")
	os.Exit(exitUnknown)
}

// install copies the running binary to dest, by way of a temporary file beside it, so dest is never partly written.
func install(dest string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	src, err := os.Open(self)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".fence-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o555); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}

func check(args []string) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	token := fs.String("token", "/var/run/secrets/kubernetes.io/serviceaccount/token", "the pod's service account token")
	ca := fs.String("ca", "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt", "the cluster's CA certificate")
	// The kubelet gives the drain of a pod deleted at once one second, and FreeSWITCH must be stopped within it.
	timeout := fs.Duration("timeout", 600*time.Millisecond, "time allowed for asking the API server")
	if err := fs.Parse(args); err != nil {
		return exitUnknown
	}
	v, err := run(*token, *ca, *timeout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "freeswitch-fence:", err)
	}
	switch v {
	case fence.FailedOver:
		return exitFailedOver
	case fence.Live:
		return exitLive
	}
	return exitUnknown
}

func run(tokenFile, caFile string, timeout time.Duration) (fence.Verdict, error) {
	server, err := fence.APIServer()
	if err != nil {
		return fence.Unknown, err
	}
	tok, err := os.ReadFile(tokenFile)
	if err != nil {
		return fence.Unknown, err
	}
	client, err := fence.Client(caFile)
	if err != nil {
		return fence.Unknown, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return fence.Check(ctx, client, server, string(tok))
}
