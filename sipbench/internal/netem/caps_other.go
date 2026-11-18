//go:build !linux

package netem

import "os/exec"

// withNetAdmin does nothing off Linux, which has no tc.
func withNetAdmin(*exec.Cmd) {}
