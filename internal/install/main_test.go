package install

import (
	"os"
	"testing"

	"github.com/cloudground-it/cloudground/internal/version"
)

// The release tests sign with throwaway keys and pass them as the compiled
// ones (version.UpdatePublicKey); the real release key is left out.
func TestMain(m *testing.M) {
	restore := version.DropCompiledKeysForTest()
	code := m.Run()
	restore()
	os.Exit(code)
}
