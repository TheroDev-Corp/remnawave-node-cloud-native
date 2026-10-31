//go:build unix

package asn

import (
	"os"
	"syscall"
)

// mapFile maps the file read-only: pages are shared with the page cache and
// only the ones a lookup touches become resident.
func mapFile(f *os.File) ([]byte, func() error, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if st.Size() == 0 {
		return nil, func() error { return nil }, nil
	}
	b, err := syscall.Mmap(int(f.Fd()), 0, int(st.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, nil, err
	}
	return b, func() error { return syscall.Munmap(b) }, nil
}
