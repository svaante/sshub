package client

import (
	"os/exec"
	"syscall"
)

// dieWithParent makes cmd get SIGTERM if we die without detaching (SIGKILL,
// OOM): otherwise the transport keeps relaying for a vhci port nobody owns,
// and the server keeps the device in use, hidden from the next sshub.
//
// Linux sends the signal when the thread that started cmd ends, not the
// process. The Go runtime keeps its threads, except that it ends one when a
// goroutine that called runtime.LockOSThread returns still locked: never
// start a transport from such a goroutine.
func dieWithParent(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
