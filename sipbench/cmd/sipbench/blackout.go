package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/netem"
)

// maxBlackout bounds a blackout: a node left dark is lost until someone reaches it some other way, so the command
// refuses anything longer than a scenario could need.
const maxBlackout = 15 * time.Minute

// blackout takes the host it runs on off the network for a while: sipbench blackout -address <node IP> -for 120s.
// The runner starts it in a pod on the node (host network, NET_ADMIN), so the node goes dark as if it had lost
// power while its processes keep running. It prints when the node went dark and when it came back, which the
// runner reads from the pod's log, and lets traffic through again early on SIGTERM or SIGINT (the pod deleted).
func blackout(args []string) error {
	fs := flag.NewFlagSet("blackout", flag.ExitOnError)
	addr := fs.String("address", env("SIPBENCH_BLACKOUT_ADDRESS", ""),
		"the node's own address: the interface holding it is the one taken dark")
	dur := fs.Duration("for", 0, "how long the node stays dark")
	delay := fs.Duration("delay", 3*time.Second,
		"wait this long first, so the kubelet can report the pod running while it still can")
	_ = fs.Parse(args)
	if *addr == "" || *dur <= 0 {
		return errors.New("blackout needs -address and -for")
	}
	if *dur > maxBlackout {
		return fmt.Errorf("blackout -for %s: at most %s", *dur, maxBlackout)
	}
	iface, err := netem.IfaceWithAddress(*addr)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-time.After(*delay):
	case <-ctx.Done():
		fmt.Println("cancelled before going dark")
		return nil
	}

	b := &netem.Blackout{Iface: iface}
	if err := b.On(context.Background()); err != nil {
		return err
	}
	fmt.Printf("dark %s %s for %s\n", time.Now().UTC().Format(time.RFC3339Nano), iface, *dur)
	select {
	case <-time.After(*dur):
	case <-ctx.Done():
	}
	// Restoring must not be cut short: a node left dark stays lost.
	if err := b.Off(context.Background()); err != nil {
		return fmt.Errorf("letting traffic through again on %s: %w", iface, err)
	}
	fmt.Printf("restored %s %s\n", time.Now().UTC().Format(time.RFC3339Nano), iface)
	return nil
}
