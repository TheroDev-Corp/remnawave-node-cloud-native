package xray

import "syscall"

// Pdeathsig makes the kernel kill xray if the node dies, so a crashed node
// never leaves an orphaned xray holding the ports.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setpgid: true}
}
