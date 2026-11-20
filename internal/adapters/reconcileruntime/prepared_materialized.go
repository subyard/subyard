package reconcileruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"syscall"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
)

const materializedMetadataPrefix = "prepared/materialized/"
const materializedBlobPrefix = "prepared/materialized-bytes/"

func materializedBlobPath(content []byte) string {
	return materializedBlobPrefix + fmt.Sprintf("%x", sha256.Sum256(content))
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
	PreviewSkillCutover        bool `json:",omitempty"`
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
	d := materializedConfigs{PreviewSkillCutover: p.previewSkillCutover, Binding: p.binding, Runtime: p.runtime, Instance: p.instance, Entries: describeMaterializedArtifacts(p.entries)}
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
		if approved, guarded := p.packageIdentities[key.path]; guarded && identity != approved {
			return d, domain.ErrPlanStale
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
	p := &PreparedInstalledRuntime{owner: "materialized-files", runtime: runtime.configRuntimeBinding(), resources: runtime.installedResourceBinding(), files: map[string][]byte{}}
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
