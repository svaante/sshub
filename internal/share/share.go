// Package share loads what the server shares: /etc/sshub/devices, one
// device per line,
//
//	<vid>:<pid> [serial]
//
// Every client gets every share. Parsing is strict: a line that is not blank,
// a comment or a valid entry rejects the whole file, so a typo cannot change
// what is shared. sshub-helper parses the same file with the same rules.
package share

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/svaante/sshub/internal/sysfs"
)

// DefaultPath is the devices file.
const DefaultPath = "/etc/sshub/devices"

// hubClass is bDeviceClass of a hub. Exporting a hub would take every
// device behind it away from the server, so hubs are never shared.
const hubClass = 0x09

// Device shares a device by identity, wherever it is plugged in.
type Device struct {
	VID    string // idVendor, 4 lowercase hex digits
	PID    string // idProduct, 4 lowercase hex digits
	Serial string // exact serial number; empty matches any
}

// List is everything the server shares.
type List []Device

// The line format. sshub-helper uses the same expressions; keep them in
// sync.
var (
	skipRe   = regexp.MustCompile(`^[ \t]*(#.*)?$`)
	deviceRe = regexp.MustCompile(`^([0-9a-f]{4}):([0-9a-f]{4})([ \t]+([^ \t]+))?[ \t]*$`)
)

// Load reads the devices file at path; a missing file shares nothing. The
// file and its directory must be owned by root and not writable by group or
// others.
func Load(path string) (List, error) {
	if err := checkTrusted(filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Check the opened file, not the path, so it cannot be swapped between
	// the check and the read.
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err := trusted(path, fi); err != nil {
		return nil, err
	}
	return Parse(path, f)
}

// Parse parses a devices file read from r. name is used in error messages.
// Lines are split on "\n" only, like the helper's read: a "\r" stays part of
// the line and makes it invalid.
func Parse(name string, r io.Reader) (List, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	lines := strings.Split(string(b), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var l List
	for i, line := range lines {
		if skipRe.MatchString(line) {
			continue
		}
		m := deviceRe.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("%s:%d: invalid line %q", name, i+1, line)
		}
		l = append(l, Device{VID: m[1], PID: m[2], Serial: m[4]})
	}
	return l, nil
}

// Shared reports whether dev matches a line. Hubs never do.
func (l List) Shared(dev sysfs.Device) bool {
	if dev.Class == hubClass {
		return false
	}
	for _, d := range l {
		if d.VID == fmt.Sprintf("%04x", dev.VendorID) &&
			d.PID == fmt.Sprintf("%04x", dev.ProductID) &&
			(d.Serial == "" || d.Serial == dev.Serial) {
			return true
		}
	}
	return false
}

func checkTrusted(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	return trusted(path, fi)
}

// trusted refuses files that someone other than root could have written.
func trusted(path string, fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: cannot determine owner", path)
	}
	if st.Uid != 0 {
		return fmt.Errorf("%s: must be owned by root", path)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s: must not be group or world writable", path)
	}
	return nil
}
