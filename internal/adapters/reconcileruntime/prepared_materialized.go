package reconcileruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

const materializedMetadataPrefix = "prepared/materialized/"
const materializedBlobPrefix = "prepared/materialized-bytes/"

func materializedBlobPath(content []byte) string {
	return materializedBlobPrefix + fmt.Sprintf("%x", sha256.Sum256(content))
}

func (p *PreparedInstalledRuntime) MaterializedSourceBinding() string {
	if p == nil || (p.owner != "materialized-files" && p.owner != "materialized-integrations") {
		return ""
	}
	return p.source
}

// These descriptors are an encoding of the owning component's selection, not
// executable path authority. Bind compares them with the current closed selector
// before attaching any source reader or permitting a guest mutation.
type materializedConfigFile struct {
	Integration, Label, OwnedFormat     string
	Source, Destination, PreviewSource  []byte
	FollowSymlinks, PreviewInstructions bool
	Payload                             string
}

type materializedSourceGuard struct {
	Path           []byte
	Identity       string
	Payload        string
	Follow, Exists bool
	Projected      bool
}

type materializedConfigs struct {
	Binding, Runtime, Instance string
	Files                      []materializedConfigFile
	Entries                    []materializedArtifact
	Sources                    []materializedSourceGuard
}

func (d materializedConfigs) semantic() materializedConfigs {
	d.Sources = slices.Clone(d.Sources)
	for index := range d.Sources {
		if d.Sources[index].Projected {
			d.Sources[index].Identity = "projected"
		}
	}
	return d
}

func (d materializedConfigs) sourceBinding() string { return installedRuntimeDigest(d.semantic()) }

func (runtime Runtime) materializedProjectedSource(path string) (bool, error) {
	if runtime.PreparedSources == nil {
		return false, nil
	}
	snapshot, matched, err := runtime.PreparedSources.Snapshot(path)
	return matched && snapshot.Exists && snapshot.Identity == (config.PersistentFileIdentity{}), err
}

func (runtime Runtime) readPublishedMaterializedSource(path string) ([]byte, error) {
	files := runtime.PreparedSources
	if files == nil {
		return nil, domain.ErrPlanStale
	}
	relative, err := filepath.Rel(files.Root, path)
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, "../") {
		return nil, domain.ErrPlanStale
	}
	var snapshot config.PersistentFileSnapshot
	if files.PinnedRoot != nil {
		actual, err := os.Lstat(files.Root)
		if err != nil {
			return nil, domain.ErrPlanStale
		}
		held, err := files.PinnedRoot.Stat()
		if err != nil || !os.SameFile(actual, held) || actual.Mode()&os.ModeSymlink != 0 {
			return nil, domain.ErrPlanStale
		}
		snapshot, err = config.ReadPersistentSnapshotAt(files.PinnedRoot, filepath.ToSlash(relative))
		if err != nil {
			return nil, err
		}
	} else {
		snapshot, err = config.ReadPersistentFileSnapshot(files.Root, path)
		if err != nil {
			return nil, err
		}
	}
	if !snapshot.Exists {
		return nil, domain.ErrPlanStale
	}
	return snapshot.Content, nil
}

func (runtime Runtime) checkProjectedMaterializedGuard(path []byte, original, current string, projected bool, payload []byte) error {
	if original == current {
		return nil
	}
	if !projected {
		return domain.ErrPlanStale
	}
	actual, err := runtime.readPublishedMaterializedSource(string(path))
	if err != nil || !bytes.Equal(actual, payload) {
		return domain.ErrPlanStale
	}
	return nil
}

type materializedRecipe struct {
	Version      int
	Configs      materializedConfigs
	Integrations *materializedIntegrations `json:",omitempty"`
}

type materializedArtifact struct {
	ID, Kind, Digest, SourceDigest, Format, Content string
	Path, Target                                    []byte
	FileTarget                                      bool
	Mode                                            int
}

func describeMaterializedArtifacts(entries []integrationArtifact) []materializedArtifact {
	var result []materializedArtifact
	for _, e := range entries {
		result = append(result, materializedArtifact{e.ID, e.Kind, e.Digest, e.SourceDigest, e.Format, e.Content, []byte(e.Path), []byte(e.Target), e.FileTarget, e.Mode})
	}
	return result
}

func restoreMaterializedArtifacts(entries []materializedArtifact) []integrationArtifact {
	var result []integrationArtifact
	for _, e := range entries {
		result = append(result, integrationArtifact{ID: e.ID, Kind: e.Kind, Digest: e.Digest, SourceDigest: e.SourceDigest, Format: e.Format, Content: e.Content, Path: string(e.Path), Target: string(e.Target), FileTarget: e.FileTarget, Mode: e.Mode})
	}
	return result
}

func materializedSourceIdentity(path string) (string, error) {
	if path == "" {
		return "absent", nil
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return "absent", nil
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("materialized source must be regular")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("materialized source identity unavailable")
	}
	link, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	linked, ok := link.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("materialized link identity unavailable")
	}
	target := ""
	if link.Mode()&os.ModeSymlink != 0 {
		target, err = os.Readlink(path)
		if err != nil {
			return "", err
		}
	}
	return installedRuntimeDigest(struct {
		Device, Inode  uint64
		UID, GID       uint32
		Mode           os.FileMode
		Size, Modified int64
		Changed        syscall.Timespec
		Link           []byte
		LinkIdentity   struct {
			Device, Inode uint64
			Mode          os.FileMode
			Modified      int64
		}
	}{Device: st.Dev, Inode: st.Ino, UID: st.Uid, GID: st.Gid, Mode: info.Mode(), Size: info.Size(), Modified: info.ModTime().UnixNano(), Changed: st.Ctim, Link: []byte(target), LinkIdentity: struct {
		Device, Inode uint64
		Mode          os.FileMode
		Modified      int64
	}{linked.Dev, linked.Ino, link.Mode(), link.ModTime().UnixNano()}}), nil
}

func (runtime Runtime) materializedConfigDescription(p *PreparedConfigs) (materializedConfigs, error) {
	d := materializedConfigs{Binding: p.binding, Runtime: p.runtime, Instance: p.instance, Entries: describeMaterializedArtifacts(p.entries)}
	for _, f := range p.files {
		file := materializedConfigFile{Integration: f.integration, Label: f.label, Source: []byte(runtime.bindingPath(f.source)), Destination: []byte(f.destination), PreviewSource: []byte(runtime.bindingPath(f.previewSource)), OwnedFormat: f.ownedFormat, FollowSymlinks: f.followSymlinks, PreviewInstructions: f.previewInstructions}
		if payload, exists := p.payloads[f.destination]; exists {
			file.Payload = materializedBlobPath(payload)
		}
		d.Files = append(d.Files, file)
	}
	for key, value := range p.sources {
		identity, err := materializedSourceIdentity(key.path)
		if err != nil {
			return d, err
		}
		payload := ""
		if value.exists {
			payload = materializedBlobPath(value.payload)
		}
		projected, err := runtime.materializedProjectedSource(key.path)
		if err != nil {
			return d, err
		}
		d.Sources = append(d.Sources, materializedSourceGuard{[]byte(runtime.bindingPath(key.path)), identity, payload, key.follow, value.exists, projected})
	}
	slices.SortFunc(d.Sources, func(a, b materializedSourceGuard) int {
		if result := bytes.Compare(a.Path, b.Path); result != 0 {
			return result
		}
		if a.Follow == b.Follow {
			return 0
		}
		if a.Follow {
			return 1
		}
		return -1
	})
	return d, nil
}

// PrepareMaterializedConfigs serializes the original approved payloads together
// with their exact source descriptors, inventory and native instance identity.
func (runtime Runtime) PrepareMaterializedConfigs(ctx context.Context, prepared *PreparedConfigs) (*PreparedInstalledRuntime, error) {
	if prepared == nil {
		return nil, domain.ErrPlanStale
	}
	description, err := runtime.materializedConfigDescription(prepared)
	if err != nil {
		return nil, err
	}
	p := &PreparedInstalledRuntime{owner: "materialized-files", runtime: runtime.configRuntimeBinding(), resources: runtime.installedResourceBinding(), files: map[string][]byte{}, configs: prepared}
	for index, file := range description.Files {
		if file.Payload != "" {
			p.files[file.Payload] = prepared.payloads[prepared.files[index].destination]
		}
	}
	for _, value := range prepared.sources {
		if value.exists {
			p.files[materializedBlobPath(value.payload)] = value.payload
		}
	}
	if err = writeMaterializedMetadata(p, materializedRecipe{Version: 1, Configs: description}); err != nil {
		return nil, err
	}
	p.source = description.sourceBinding()
	if err := checkMaterializedRecipeSize(p); err != nil {
		return nil, err
	}
	p.binding = installedRuntimeDigest(json.RawMessage(p.Recipe()))
	return p, nil
}

func checkMaterializedRecipeSize(p *PreparedInstalledRuntime) error {
	total := 0
	for _, content := range p.files {
		if len(content) > 32<<20 {
			return errors.New("materialized recipe leaf bound")
		}
		total += len(content)
	}
	if len(p.files) > 8192 || total > 64<<20 || len(p.Recipe()) > MaxInstalledRecipe {
		return errors.New("materialized recipe source bound")
	}
	return nil
}

func writeMaterializedMetadata(p *PreparedInstalledRuntime, value materializedRecipe) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(raw) > 64<<20 {
		return errors.New("materialized metadata bound")
	}
	for offset, index := 0, 0; offset < len(raw); index++ {
		end := min(len(raw), offset+(32<<20))
		p.files[fmt.Sprintf("%s%04d", materializedMetadataPrefix, index)] = raw[offset:end]
		offset = end
	}
	return nil
}

func readMaterializedMetadata(p *PreparedInstalledRuntime) ([]byte, map[string]bool, error) {
	var raw []byte
	allowed := map[string]bool{}
	for index := 0; index < 2; index++ {
		path := fmt.Sprintf("%s%04d", materializedMetadataPrefix, index)
		part, exists := p.files[path]
		if !exists {
			break
		}
		if index != 0 && len(raw) != 32<<20 || len(part) > 32<<20 {
			return nil, nil, errors.New("invalid materialized metadata chunks")
		}
		raw = append(raw, part...)
		allowed[path] = true
	}
	return raw, allowed, nil
}

func (runtime Runtime) restoreMaterializedConfigs(ctx context.Context, p *PreparedInstalledRuntime) (*PreparedConfigs, error) {
	var original materializedRecipe
	raw, allowed, err := readMaterializedMetadata(p)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&original); err != nil {
		return nil, err
	}
	canonical, _ := json.Marshal(original)
	if original.Version != 1 || !bytes.Equal(raw, canonical) {
		return nil, errors.New("invalid materialized metadata")
	}
	if p.owner == "materialized-files" && original.Integrations != nil {
		return nil, errors.New("unexpected integration metadata")
	}
	if err := p.overlayAfterOwnerConfigs(&original); err != nil {
		return nil, err
	}
	if p.afterOwnerConfigs != nil || p.afterOwnerConfigSource {
		if (p.owner != "materialized-files" && p.owner != "materialized-integrations") || p.afterOwner == nil {
			return nil, domain.ErrPlanStale
		}
	}
	// The selector is only a scope/guard oracle. Its newly read payloads are
	// discarded; the returned handle below is populated from original bytes.
	var fresh *PreparedConfigs
	if p.afterOwnerConfigSource {
		if original.Configs.Instance != afterOwnerConfigInstance {
			return nil, domain.ErrPlanStale
		}
		fresh, err = runtime.prepareConfigPayloads(afterOwnerConfigInstance)
	} else if p.afterOwnerConfigs != nil {
		fresh, err = runtime.afterOwnerPhysicalConfigs(ctx)
	} else {
		fresh, err = runtime.PrepareConfigs(ctx)
	}
	if err != nil {
		return nil, err
	}
	description, err := runtime.materializedConfigDescription(fresh)
	if err != nil {
		return nil, err
	}
	if len(description.Sources) != len(original.Configs.Sources) {
		return nil, domain.ErrPlanStale
	}
	for index, guard := range original.Configs.Sources {
		current := &description.Sources[index]
		if !bytes.Equal(current.Path, guard.Path) || current.Projected != guard.Projected {
			return nil, domain.ErrPlanStale
		}
		if err := runtime.checkProjectedMaterializedGuard(guard.Path, guard.Identity, current.Identity, guard.Projected, p.files[guard.Payload]); err != nil {
			return nil, err
		}
		current.Identity = guard.Identity
	}
	if installedRuntimeDigest(description) != installedRuntimeDigest(original.Configs) || p.owner == "materialized-files" && p.afterOwnerConfigs == nil && p.source != original.Configs.sourceBinding() {
		return nil, domain.ErrPlanStale
	}
	for index, file := range original.Configs.Files {
		if file.Payload == "" {
			continue
		}
		payload, exists := p.files[file.Payload]
		if !exists || !bytes.Equal(payload, fresh.payloads[fresh.files[index].destination]) {
			return nil, domain.ErrPlanStale
		}
		allowed[file.Payload] = true
		fresh.payloads[fresh.files[index].destination] = payload
	}
	for _, source := range original.Configs.Sources {
		if source.Exists {
			payload, exists := p.files[source.Payload]
			if !exists || source.Payload != materializedBlobPath(payload) {
				return nil, domain.ErrPlanStale
			}
			allowed[source.Payload] = true
			for key := range fresh.sources {
				if key.follow == source.Follow && bytes.Equal([]byte(runtime.bindingPath(key.path)), source.Path) {
					fresh.sources[key] = configSourceBytes{payload: payload, exists: true}
				}
			}
		}
	}
	if p.owner == "materialized-files" && len(allowed) != len(p.files) {
		return nil, errors.New("unrecognized materialized recipe leaf")
	}
	fresh.entries = restoreMaterializedArtifacts(original.Configs.Entries)
	return fresh, nil
}

func (runtime Runtime) bindMaterializedRecipe(ctx context.Context, r installedRecipe, raw []byte) (*PreparedInstalledRuntime, error) {
	if r.Profile != "" || r.Handler != "" || r.Launcher != "" || r.HookBefore != "" || len(r.Native) != 0 || len(r.Leaves) != 0 || (r.Owner != "materialized-files" && r.Owner != "materialized-integrations") {
		return nil, errors.New("invalid materialized recipe scope")
	}
	p := &PreparedInstalledRuntime{owner: r.Owner, source: r.Source, runtime: r.Runtime, resources: r.Resources, files: map[string][]byte{}, afterOwnerConfigs: runtime.afterOwnerConfigs, afterOwnerConfigSource: runtime.afterOwnerConfigSource, afterOwner: r.AfterOwner, afterOwnerIntegrations: runtime.afterOwnerIntegrations}
	previous := ""
	for _, file := range r.Files {
		if file.Path <= previous || len(file.Path) > 512 || filepath.Clean(file.Path) != file.Path || filepath.IsAbs(file.Path) {
			return nil, errors.New("invalid materialized recipe path")
		}
		previous = file.Path
		p.files[file.Path] = file.Content
	}
	if err := checkMaterializedRecipeSize(p); err != nil {
		return nil, err
	}
	if p.runtime != runtime.configRuntimeBinding() || p.resources != runtime.installedResourceBinding() {
		return nil, domain.ErrPlanStale
	}
	var err error
	p.configs, err = runtime.restoreMaterializedConfigs(ctx, p)
	if err != nil {
		return nil, err
	}
	if p.owner == "materialized-integrations" {
		if err := runtime.restoreMaterializedIntegrations(ctx, p); err != nil {
			return nil, err
		}
	}
	p.binding = installedRuntimeDigest(json.RawMessage(raw))
	return p, nil
}

func (runtime Runtime) ApplyPreparedMaterializedConfigs(ctx context.Context, p *PreparedInstalledRuntime) error {
	if p == nil || p.owner != "materialized-files" || p.closed.Load() || p.runtime != runtime.configRuntimeBinding() || p.resources != runtime.installedResourceBinding() {
		return domain.ErrPlanStale
	}
	prepared, err := runtime.restoreMaterializedConfigs(ctx, p)
	if err != nil {
		return err
	}
	// No source reads occur beyond this guard: all guest writes consume the
	// reconstructed original payloads and original source-owned destinations.
	return runtime.applyPreparedConfigs(ctx, prepared, false)
}
