package main

import (
	"fmt"
	"io"

	"github.com/cloudground-it/cloudground/internal/version"
)

// releaseCommand is `cgctl release verify <dir> [file...]` (SPEC.md §5): it
// checks a downloaded release with the signing keys compiled into this
// binary and needs no panel, token or configuration. The release build
// runs it on what it is about to publish; an operator can run it on a
// downloaded release.
func releaseCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 || args[0] != "verify" {
		fmt.Fprintln(stderr, "usage: cgctl release verify <dir> [file...]")
		return exitUsage
	}
	m, err := version.VerifyRelease(args[1], args[2:]...)
	if err != nil {
		fmt.Fprintln(stderr, "cgctl: release not verified:", err)
		return exitFailed
	}
	n := len(args) - 2
	if n == 0 {
		n = len(m.Sums)
	}
	fmt.Fprintf(stdout, "%s: signature and %d file(s) verified\n", m.Version, n)
	return exitOK
}
