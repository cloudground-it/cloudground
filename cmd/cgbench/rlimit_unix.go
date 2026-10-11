//go:build unix

package main

import "syscall"

// raiseFileLimit lifts this process's soft RLIMIT_NOFILE to want, or to the
// hard limit when that is lower. wrk inherits it (bench/SPEC.md §2).
func raiseFileLimit(want uint64) error {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return err
	}
	if lim.Cur >= want {
		return nil
	}
	target := want
	if lim.Max < target {
		// root may raise the hard limit too; anyone else stops at it.
		lim.Max = target
		lim.Cur = target
		if syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim) == nil {
			return nil
		}
		if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
			return err
		}
		target = lim.Max
	}
	lim.Cur = target
	return syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim)
}
