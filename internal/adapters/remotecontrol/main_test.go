package remotecontrol

import (
	"os"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

// Owner coordination in tests uses a private lock root removed after the run.
func TestMain(m *testing.M) { os.Exit(testkit.RunWithOwnerLockRoot(m)) }
