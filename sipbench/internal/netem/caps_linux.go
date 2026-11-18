package netem

import (
	"os/exec"
	"syscall"
)

// capNetAdmin is CAP_NET_ADMIN (linux/capability.h).
const capNetAdmin = 12

// withNetAdmin hands NET_ADMIN to cmd as an ambient capability, so tc and ip have it even when the runner is not
// root: the runner's image gives its binary the capability (permitted), and ip keeps an inherited one.
func withNetAdmin(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{AmbientCaps: []uintptr{capNetAdmin}}
}
