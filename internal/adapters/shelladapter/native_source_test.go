package shelladapter

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestNativeOwnerSourcePreparationRequiresForegroundDrain(t *testing.T) {
	for _, mode := range []string{"drained", "survivor", "missing-apply-admission"} {
		t.Run(mode, func(t *testing.T) {
			root := testkit.TempDir(t)
			body := "#!/bin/sh\nset -eu\nprintf dispatched >\"$FIXTURE/effect\"\n"
			if mode == "survivor" {
				body += "sleep 300 >/dev/null 2>&1 &\nprintf '%s\\n' \"$!\" >\"$FIXTURE/child.pid\"\n"
			}
			path := filepath.Join(root, "engine")
			testkit.WriteFile(t, path, []byte(body), 0o700)
			image, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer image.Close()
			sum := sha256.Sum256([]byte(body))
			ctx := ports.WithNativePreparationExecutable(context.Background(), fixedSelfTestCapability{image, fmt.Sprintf("%x", sum)})
			peer, remote, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			defer remote.Close()
			t.Cleanup(func() {
				if raw, err := os.ReadFile(filepath.Join(root, "child.pid")); err == nil {
					pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
					if err != nil || pid <= 1 {
						t.Errorf("invalid fixture child PID %q", raw)
						return
					}
					child, err := os.FindProcess(pid)
					if err != nil {
						t.Error(err)
						return
					}
					_ = child.Kill()
				}
			})
			leaf := "capture"
			if mode == "missing-apply-admission" {
				leaf = "apply"
			}
			exchanged := false
			err = RunNativeOwnerSource(ctx, leaf, []string{"PATH=/usr/bin:/bin", "FIXTURE=" + root}, root, peer, nil, func() error {
				exchanged = true
				return nil
			})
			if mode == "drained" {
				if err != nil || !exchanged {
					t.Fatalf("drained preparation: exchange=%v err=%v", exchanged, err)
				}
			} else if mode == "survivor" {
				if err == nil || !strings.Contains(err.Error(), "foreground is not drained") || !exchanged {
					t.Fatalf("surviving preparation: exchange=%v err=%v", exchanged, err)
				}
			} else {
				_, effectErr := os.Stat(filepath.Join(root, "effect"))
				if err == nil || !strings.Contains(err.Error(), "admission unavailable") || exchanged || !os.IsNotExist(effectErr) {
					t.Fatalf("missing apply admission: exchange=%v effect=%v err=%v", exchanged, effectErr, err)
				}
			}
		})
	}
}
