//go:build !linux

package client

import "os/exec"

// dieWithParent does nothing: only Linux can tie a child's life to ours, and
// the client only attaches devices on Linux anyway.
func dieWithParent(*exec.Cmd) {}
