package netem

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// fakeTC records the commands it is given, and fails those that start with a prefix in fail, with that error.
type fakeTC struct {
	ran  []string
	fail map[string]string
}

func (f *fakeTC) exec(_ context.Context, args ...string) error {
	cmd := strings.Join(args, " ")
	f.ran = append(f.ran, cmd)
	for prefix, msg := range f.fail {
		if strings.HasPrefix(cmd, prefix) {
			return errors.New(cmd + ": exit status 2: " + msg)
		}
	}
	return nil
}

func TestBlackoutAddsAndRemovesItsQdisc(t *testing.T) {
	f := &fakeTC{}
	b := &Blackout{Iface: "eth0", Exec: f.exec}
	if err := b.On(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b.Off(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b.Off(context.Background()); err != nil { // twice is harmless
		t.Fatal(err)
	}
	want := []string{
		"tc qdisc add dev eth0 clsact",
		"tc filter add dev eth0 ingress pref 1 matchall action drop",
		"tc filter add dev eth0 egress pref 1 matchall action drop",
		"tc qdisc del dev eth0 clsact",
		"tc filter del dev eth0 ingress pref 1",
		"tc filter del dev eth0 egress pref 1",
	}
	if !slices.Equal(f.ran, want) {
		t.Errorf("ran\n%s\nwant\n%s", strings.Join(f.ran, "\n"), strings.Join(want, "\n"))
	}
}

func TestBlackoutKeepsSomeoneElsesQdisc(t *testing.T) {
	f := &fakeTC{fail: map[string]string{"tc qdisc add": "Error: Exclusivity flag on, cannot modify."}}
	b := &Blackout{Iface: "eth0", Exec: f.exec}
	if err := b.On(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.ran = nil
	if err := b.Off(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"tc filter del dev eth0 ingress pref 1", "tc filter del dev eth0 egress pref 1"}
	if !slices.Equal(f.ran, want) {
		t.Errorf("Off ran %q, want %q", f.ran, want)
	}
}

func TestBlackoutUndoesAFailedOn(t *testing.T) {
	f := &fakeTC{fail: map[string]string{"tc filter add dev eth0 egress": "Error: Unknown filter \"matchall\"."}}
	b := &Blackout{Iface: "eth0", Exec: f.exec}
	if err := b.On(context.Background()); err == nil {
		t.Fatal("On succeeded with a failing filter")
	}
	if last := f.ran[len(f.ran)-1]; last != "tc qdisc del dev eth0 clsact" {
		t.Errorf("after a failed On, last ran %q; want the qdisc removed", last)
	}
}

func TestIfaceWithAddress(t *testing.T) {
	if name, err := IfaceWithAddress("127.0.0.1"); err != nil || name == "" {
		t.Errorf("loopback: %q, %v", name, err)
	}
	if _, err := IfaceWithAddress("192.0.2.77"); err == nil {
		t.Error("found an interface for an address no interface holds")
	}
	if _, err := IfaceWithAddress("eth0"); err == nil {
		t.Error("accepted a name for an address")
	}
}
