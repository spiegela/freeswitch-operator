package netem

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
)

// blackoutPref is the tc filter priority a blackout's drop filters take: 1, ahead of any filter already on the
// interface, so nothing gets past them. Off removes only filters at this priority.
const blackoutPref = "1"

// Blackout takes a host off the network: every frame its interface sends or receives is dropped, ARP included,
// as if the host had lost power, while the host and its processes keep running. It is how sipbench makes an edge
// node go dark (the node-lost scenarios); the command that runs it, sipbench blackout, runs on the node itself.
//
// It uses tc's clsact qdisc, with a matchall filter dropping everything in each direction. The qdisc is left in
// place if it was there before (another tool's filters may hang off it), and removed otherwise.
type Blackout struct {
	// Iface is the interface to take dark (see IfaceWithAddress).
	Iface string
	// Exec runs a command (tc); nil runs it for real.
	Exec func(ctx context.Context, args ...string) error

	addedQdisc bool
}

// On drops everything on the interface from now on.
func (b *Blackout) On(ctx context.Context) error {
	sh := &Shaper{Exec: b.Exec}
	err := sh.exec(ctx, "tc", "qdisc", tcAdd, "dev", b.Iface, "clsact")
	switch {
	case err == nil:
		b.addedQdisc = true
	case strings.Contains(err.Error(), "File exists") || strings.Contains(err.Error(), "Exclusivity flag on"):
		// Someone else's clsact: hang our filters off it, and leave it there afterwards.
	default:
		return err
	}
	for _, dir := range []string{"ingress", "egress"} {
		if err := sh.exec(ctx, "tc", "filter", tcAdd, "dev", b.Iface, dir, "pref", blackoutPref,
			"matchall", "action", "drop"); err != nil {
			return errors.Join(err, b.Off(context.WithoutCancel(ctx)))
		}
	}
	return nil
}

// Off lets traffic through again. It removes what On added and is safe to call more than once, or after a
// failed On.
func (b *Blackout) Off(ctx context.Context) error {
	sh := &Shaper{Exec: b.Exec}
	var errs []error
	if b.addedQdisc {
		// Deleting the qdisc takes its filters with it.
		if err := sh.exec(ctx, "tc", "qdisc", tcDel, "dev", b.Iface, "clsact"); err != nil && !missing(err) {
			errs = append(errs, err)
		}
		b.addedQdisc = false
		return errors.Join(errs...)
	}
	for _, dir := range []string{"ingress", "egress"} {
		err := sh.exec(ctx, "tc", "filter", tcDel, "dev", b.Iface, dir, "pref", blackoutPref)
		if err != nil && !missing(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// missing reports whether tc failed because what it was asked to delete isn't there.
func missing(err error) bool {
	s := err.Error()
	return strings.Contains(s, "No such file") || strings.Contains(s, "Cannot find") ||
		strings.Contains(s, "Invalid handle") || strings.Contains(s, "Filter with specified priority/protocol not found")
}

// IfaceWithAddress returns the interface that holds ip: on a node, the one its own address is on, which carries
// everything the node sends to and receives from the rest of the cluster.
func IfaceWithAddress(ip string) (string, error) {
	want := net.ParseIP(ip)
	if want == nil {
		return "", fmt.Errorf("%q is not an IP address", ip)
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.Equal(want) {
				return ifc.Name, nil
			}
		}
	}
	return "", fmt.Errorf("no interface holds %s", ip)
}
