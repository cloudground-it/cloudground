//go:build linux

package early

import "testing"

// Importing the package is enough: by the time a test runs, the process is
// no longer dumpable.
func TestNotDumpableAfterInit(t *testing.T) {
	d, err := Dumpable()
	if err != nil {
		t.Fatal(err)
	}
	if d {
		t.Fatal("the process is still dumpable after early's init")
	}
}
