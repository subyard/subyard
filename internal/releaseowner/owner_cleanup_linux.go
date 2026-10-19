package releaseowner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Subyard/Subyard/internal/ports"
	"golang.org/x/sys/unix"
)

// queryOwnerCleanup closes only the audited helper-local terminal archive
// callback. A newly acquired config/update/account lease excludes its original
// mutating thread; surviving fixed read-only inventory queries have no effect
// authority. No generic SDK/script invocation can use this local proof.
func (s *session) queryOwnerCleanup(_ context.Context, r Request) (Reply, error) {
	if r.Before == nil || len(r.Token) != 32 || strings.Trim(r.Token, "0123456789abcdef") != "" || r.Binding == nil || s.binding == nil || *r.Binding != *s.binding || len(s.leases) != 2 || s.ownerRelease == nil {
		return Reply{Code: "unknown"}, nil
	}
	if _, ok := s.components.(OwnerArchiveCleaner); !ok {
		return Reply{Code: "unknown"}, nil
	}
	recipe, err := recipeFingerprint(r)
	if err != nil {
		return Reply{}, err
	}
	if err := s.checkIntent(*r.Binding, "owner-registration-cleanup", "test-yard", recipe); err != nil {
		return Reply{Code: "unknown"}, nil
	}
	directory, err := s.factsDirectory(*r.Binding)
	if err != nil {
		return Reply{}, err
	}
	defer unix.Close(directory)
	accepted := Terminal{Version: 1, Binding: *r.Binding, Owner: "owner-registration-cleanup", Resource: "test-yard", State: "accepted", Operations: []ports.NativeOperation{}}
	raw, err := readFactAt(directory, factName(r.Binding.Ordinal, "accepted"))
	expected, _ := json.Marshal(accepted)
	if err != nil || !bytes.Equal(raw, append(expected, '\n')) {
		return Reply{Code: "unknown"}, nil
	}
	fd, err := unix.Openat(directory, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return Reply{}, err
	}
	file := os.NewFile(uintptr(fd), "local cleanup fact inventory")
	defer file.Close()
	names, err := file.Readdirnames(8193)
	if err != nil && !errors.Is(err, io.EOF) {
		return Reply{}, err
	}
	if len(names) > 8192 {
		return Reply{Code: "unknown"}, nil
	}
	prefix := fmt.Sprintf("%03d.", r.Binding.Ordinal)
	for _, name := range names {
		candidate := strings.TrimPrefix(name, ".")
		if !strings.HasPrefix(candidate, prefix) {
			continue
		}
		allowed := false
		for _, kind := range []string{"reservation", "intent", "accepted", "terminal"} {
			base := factName(r.Binding.Ordinal, kind)
			allowed = allowed || name == base || name == "."+base+".pending"
		}
		if !allowed {
			return Reply{Code: "unknown"}, nil
		}
	}
	if terminal, err := s.readTerminal(*r.Binding); err == nil {
		return Reply{Terminal: terminal}, nil
	}
	terminal := accepted
	terminal.State = "failed"
	if pending, err := readFactAt(directory, "."+factName(r.Binding.Ordinal, "terminal")+".pending"); err == nil {
		var prior Terminal
		if strict(pending, &prior) != nil || prior.Binding != terminal.Binding || prior.Version != 1 || prior.Owner != terminal.Owner || prior.Resource != terminal.Resource || (prior.State != "complete" && prior.State != "failed") || len(prior.Operations) != 0 {
			return Reply{Code: "unknown"}, nil
		}
		expectedPrior := accepted
		expectedPrior.State = prior.State
		canonical, _ := json.Marshal(expectedPrior)
		if !bytes.Equal(pending, append(canonical, '\n')) {
			return Reply{Code: "unknown"}, nil
		}
		terminal = prior
	} else if !errors.Is(err, unix.ENOENT) {
		return Reply{Code: "unknown"}, nil
	}
	if err := s.writeTerminal(terminal); err != nil {
		return Reply{Code: "unknown"}, nil
	}
	return Reply{Terminal: &terminal}, nil
}
