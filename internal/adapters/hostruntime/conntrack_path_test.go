package hostruntime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/testkit"
)

func TestFindConntrackUsesAbsolutePATHThenSystemFallback(t *testing.T) {
	pathDirectory := testkit.TempDir(t)
	systemDirectory := testkit.TempDir(t)
	systemBinary := filepath.Join(systemDirectory, "conntrack")
	testkit.WriteFile(t, systemBinary, []byte("#!/bin/sh\n"), 0o700)
	if found, ok := FindConntrack(pathDirectory, systemDirectory); !ok || found != systemBinary {
		t.Fatalf("system sbin fallback: path=%q found=%t", found, ok)
	}
	pathBinary := filepath.Join(pathDirectory, "conntrack")
	testkit.WriteFile(t, pathBinary, []byte("#!/bin/sh\n"), 0o700)
	if found, ok := FindConntrack(pathDirectory, systemDirectory); !ok || found != pathBinary {
		t.Fatalf("absolute PATH did not take precedence: path=%q found=%t", found, ok)
	}
	if found, ok := FindConntrack(testkit.TempDir(t)); ok || found != "" {
		t.Fatalf("missing isolated tool was accepted: path=%q found=%t", found, ok)
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relativeDirectory, err := filepath.Rel(workingDirectory, pathDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if found, ok := FindConntrack(relativeDirectory); ok || found != "" {
		t.Fatalf("relative PATH executable was accepted: path=%q found=%t", found, ok)
	}
}
