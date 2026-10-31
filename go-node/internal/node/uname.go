//go:build unix

package node

import "golang.org/x/sys/unix"

// uname returns os.release() and os.version() (uname -r / uname -v).
func uname() (release, version string) {
	var u unix.Utsname
	if unix.Uname(&u) != nil {
		return "", ""
	}
	return unix.ByteSliceToString(u.Release[:]), unix.ByteSliceToString(u.Version[:])
}
