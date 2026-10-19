package testvmsruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"syscall"

	"github.com/Subyard/Subyard/internal/domain"
	"golang.org/x/sys/unix"
)

// PreparedApplyRecord contains only the owning backend's original plan and
// fixed payload references. Its binary leaves are stored separately so the
// release bundle can deduplicate their original bytes across consumers.
type PreparedApplyRecord struct {
	Version                    int
	Inputs, Native, Binding    string
	State                      [][]byte
	Payloads                   map[string]string
	EngineRole, EngineIdentity string
}

func backendStateValues(s backendState, label func(string) string) [][]byte {
	values := []string{s.diskBudget, s.cacheBudget, s.diskReserve, s.memoryReserve, s.vmOverhead, s.enabled, s.cpu, s.image, s.memory, s.disk, s.slotCount, s.bootTimeout, s.brokerSource, s.engineHash, s.marker, s.clientDirectory, s.provision, s.downloadHelper}
	result := make([][]byte, len(values))
	for i, value := range values {
		result[i] = []byte(label(value))
	}
	return result
}

func (p *PreparedApply) Record(backend *Backend) (PreparedApplyRecord, map[string][]byte, error) {
	if p == nil || backend == nil {
		return PreparedApplyRecord{}, nil, domain.ErrPlanStale
	}
	r := PreparedApplyRecord{Version: 1, Inputs: p.inputs, Native: p.native, Binding: p.binding, State: backendStateValues(p.state, backend.bindingPath), Payloads: map[string]string{}}
	if err := p.checkEngine(); err != nil {
		return r, nil, err
	}
	files := map[string][]byte{"provision": p.provision, "download": p.download, "recipes": p.recipes, "manifest": p.manifest}
	original, err := os.Stat(backend.Dispatcher)
	if err != nil {
		return r, nil, err
	}
	fixed := filepath.Join(backend.RepositoryRoot, "bin", "yard-engine")
	asset, err := os.Stat(fixed)
	if err == nil && os.SameFile(original, asset) {
		r.EngineRole = "yard-engine"
		r.EngineIdentity, err = preparedEngineIdentity(fixed)
		if err != nil {
			return r, nil, err
		}
		digest, err := fileSHA256(fixed)
		if err != nil || digest != p.state.engineHash {
			return r, nil, domain.ErrPlanStale
		}
		r.Payloads["engine"] = p.state.engineHash
	} else {
		engine, err := io.ReadAll(io.NewSectionReader(p.engine, 0, (128<<20)+1))
		if err != nil || len(engine) > 128<<20 {
			return r, nil, errors.New("prepared broker engine payload capacity")
		}
		files["engine"] = engine
	}
	for name, payload := range files {
		if name != "engine" && len(payload) > 32<<20 || len(payload) > 128<<20 {
			return r, nil, errors.New("prepared broker leaf capacity")
		}
		r.Payloads[name] = fmt.Sprintf("%x", sha256.Sum256(payload))
	}
	return r, files, nil
}

func (backend *Backend) RestorePreparedApply(ctx context.Context, r PreparedApplyRecord, files map[string][]byte) (*PreparedApply, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.Version != 1 || len(r.Payloads) != 5 || r.Inputs != backend.inputBinding() || r.EngineRole != "" && r.EngineRole != "yard-engine" || r.EngineRole == "" && r.EngineIdentity != "" || r.EngineRole == "" && len(files) != 5 || r.EngineRole != "" && len(files) != 4 {
		return nil, domain.ErrPlanStale
	}
	for _, value := range []string{r.Native, r.Binding} {
		decoded, err := hex.DecodeString(value)
		if err != nil || len(decoded) != sha256.Size {
			return nil, domain.ErrPlanStale
		}
	}
	state, err := backend.state()
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(r.State, backendStateValues(state, backend.bindingPath)) {
		return nil, domain.ErrPlanStale
	}
	for _, name := range []string{"engine", "provision", "download", "recipes", "manifest"} {
		if name == "engine" && r.EngineRole != "" {
			continue
		}
		payload, ok := files[name]
		if !ok || name != "engine" && len(payload) > 32<<20 || len(payload) > 128<<20 || fmt.Sprintf("%x", sha256.Sum256(payload)) != r.Payloads[name] {
			return nil, domain.ErrPlanStale
		}
	}
	if r.Payloads["engine"] != state.engineHash {
		return nil, domain.ErrPlanStale
	}
	var engine *os.File
	if r.EngineRole == "yard-engine" {
		fixed := filepath.Join(backend.RepositoryRoot, "bin", "yard-engine")
		expected, err := os.Stat(backend.Dispatcher)
		if err != nil {
			return nil, err
		}
		actual, err := os.Stat(fixed)
		if err != nil || !os.SameFile(expected, actual) {
			return nil, domain.ErrPlanStale
		}
		identity, err := preparedEngineIdentity(fixed)
		if err != nil || identity != r.EngineIdentity {
			return nil, domain.ErrPlanStale
		}
		input, err := os.OpenFile(fixed, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return nil, err
		}
		defer input.Close()
		engine, err = SealPreparedEngine(input, r.Payloads["engine"])
		if err != nil {
			return nil, err
		}
		after, err := preparedEngineIdentity(fixed)
		if err != nil || after != r.EngineIdentity {
			engine.Close()
			return nil, domain.ErrPlanStale
		}
	} else {
		fd, err := unix.MemfdCreate("subyard-original-broker", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
		if err != nil {
			return nil, err
		}
		input := os.NewFile(uintptr(fd), "original broker bytes")
		defer input.Close()
		if _, err := input.Write(files["engine"]); err != nil {
			return nil, err
		}
		engine, err = SealPreparedEngine(input, state.engineHash)
		if err != nil {
			return nil, err
		}
	}
	p := &PreparedApply{state: state, inputs: r.Inputs, native: r.Native, binding: r.Binding, provision: files["provision"], download: files["download"], recipes: files["recipes"], manifest: files["manifest"], engine: engine}
	// The private record is canonical and the binding must still derive from
	// these exact original leaves, never from current installed observations.
	raw, _ := json.Marshal(struct {
		Inputs, Native, Marker        string
		Provision, Download, Manifest string
	}{p.inputs, p.native, state.marker, r.Payloads["provision"], r.Payloads["download"], r.Payloads["manifest"]})
	if fmt.Sprintf("%x", sha256.Sum256(raw)) != r.Binding {
		engine.Close()
		return nil, domain.ErrPlanStale
	}
	return p, nil
}

func preparedEngineIdentity(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Size() > 128<<20 {
		return "", domain.ErrPlanStale
	}
	raw, _ := json.Marshal([]any{info.Mode(), stat.Dev, stat.Ino, stat.Uid, stat.Gid, stat.Size, stat.Mtim, stat.Ctim})
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}
