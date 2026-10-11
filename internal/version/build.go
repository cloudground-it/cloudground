// Package version is the build's identity and the rules for replacing it
// with a signed release (SPEC.md).
package version

import "fmt"

// Version is the release tag, set by the linker (SPEC §1).
var Version = "dev"

// HandleFlag answers a version or help request in args and reports whether
// the process should now exit (SPEC §1).
func HandleFlag(args []string, name, summary string) bool {
	for _, arg := range args {
		switch arg {
		case "-v", "--version", "version":
			fmt.Println(name, Version)
		case "-h", "--help", "help":
			fmt.Printf("%s %s\n\n%s\n", name, Version, summary)
		default:
			continue
		}
		return true
	}
	return false
}
