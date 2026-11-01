package testvmsruntime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

// Match invoke.sh's ELF/shell distinction, without treating an unknown shell
// worker as a compatible legacy implementation.
const sourceWorkerStatusProbe = `set -eu
w=$1
[ ! -L "$w" ] && [ -f "$w" ] && [ -x "$w" ]
[ "$(stat -c %u "$w")" = 0 ] && [ "$((0$(stat -c %a "$w") & 022))" = 0 ]
magic=$(od -An -tx1 -N4 "$w" | tr -d ' \n')
case "$magic" in
  7f454c46) printf 'elf\n' ;;
  2321????) printf 'shell '; sha256sum "$w" | cut -d ' ' -f 1 ;;
  *) exit 1 ;;
esac
`

const sourceWorkerStatusRead = `set -eu
w=$1
[ ! -L "$w" ]
exec 3<"$w"
f=/proc/self/fd/3
[ -f "$f" ] && [ -x "$f" ]
[ "$(stat -Lc %u "$f")" = 0 ] && [ "$((0$(stat -Lc %a "$f") & 022))" = 0 ]
[ "$(sha256sum "$f" | cut -d ' ' -f 1)" = "$2" ]
exec "$f" status
`

// ReadSourceWorkerStatus reads only an original source installation's worker.
// ELF retains the normal modern status path; shell requires exact source bytes.
// The owning migration caller retains its existing legacy lease decoder.
func ReadSourceWorkerStatus(ctx context.Context, sourceRoot string, yard domain.Context, executor ports.YardExecutor) ([]byte, bool, error) {
	if sourceRoot == "" {
		return nil, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ctx = ports.WithoutNativeOperationObserver(ctx)
	refuse := errors.New("original test VM worker status could not be verified")
	probe, err := executor.Execute(ctx, yard, ports.InstanceExecRequest{Command: []string{"sh", "-c", sourceWorkerStatusProbe, "source-worker-status", DefaultInstalledPath}})
	if err != nil || probe.ExitCode != 0 || len(probe.Stdout) > 128 {
		return nil, false, refuse
	}
	if string(probe.Stdout) == "elf\n" {
		return nil, false, nil
	}
	file, err := os.OpenFile(filepath.Join(sourceRoot, "scripts/e2e-lab/worker.sh"), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, false, refuse
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, false, refuse
	}
	raw, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	want := fmt.Sprintf("%x", sha256.Sum256(raw))
	if err != nil || len(raw) > 1<<20 || !strings.HasPrefix(string(raw), "#!") || string(probe.Stdout) != "shell "+want+"\n" {
		return nil, false, refuse
	}
	result, err := executor.Execute(ctx, yard, ports.InstanceExecRequest{Command: []string{"sh", "-c", sourceWorkerStatusRead, "source-worker-status", DefaultInstalledPath, want}})
	if err != nil || result.ExitCode != 0 || len(result.Stdout) > 64<<10 {
		return nil, false, refuse
	}
	return result.Stdout, true, nil
}
