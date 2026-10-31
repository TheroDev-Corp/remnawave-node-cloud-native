//go:build unix

package xray

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// signalName is the SIGXXX name of the signal that ended the process.
func signalName(ps *os.ProcessState) string {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return unix.SignalName(ws.Signal())
	}
	return ""
}
