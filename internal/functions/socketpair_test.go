package functions

import (
	"os"
	"syscall"
)

// socketpair is two connected unix stream sockets as files.
func socketpair() ([2]*os.File, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return [2]*os.File{}, err
	}
	return [2]*os.File{os.NewFile(uintptr(fds[0]), "a"), os.NewFile(uintptr(fds[1]), "b")}, nil
}
