package github_test

import (
	"os"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

// The peer fixture's engine coordinates under a private lock root removed after the run.
func TestMain(m *testing.M) { os.Exit(testkit.RunWithOwnerLockRoot(m)) }
