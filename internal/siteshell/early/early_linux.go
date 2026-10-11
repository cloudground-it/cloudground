// Package early makes the gate client not dumpable as early as Go allows:
// cmd/cg-site-shell imports it, and it depends on package syscall alone, so
// its init runs before siteshell's, golang.org/x/sys/unix's and main's.
// Only the runtime and the standard library's own package initialisation
// (no code of this project, nothing the site controls) come before it
// (ops/sftp SPEC §5.4).
package early

import "syscall"

const prSetDumpable = 4 // PR_SET_DUMPABLE

func init() {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0); errno != 0 {
		msg := []byte("cg-site-shell: cannot make the session safe\n")
		syscall.Write(2, msg)
		syscall.Exit(1)
	}
}

// Dumpable reports the process's dumpable flag (PR_GET_DUMPABLE), for tests.
func Dumpable() (bool, error) {
	r, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, 3, 0, 0)
	if errno != 0 {
		return false, errno
	}
	return r != 0, nil
}
