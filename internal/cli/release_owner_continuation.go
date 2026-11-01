package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/releaseowner/nativejson"
	"github.com/Subyard/Subyard/internal/uninstall"
)

// These source facts live only inside the authenticated original init recipe.
// The retained source handle guards executable/config bytes; the manifest stays
// at its original installed root and is checked independently on every use.
type ownerLegacySource struct {
	Reference                                   releaseowner.CompleteSourceReference
	Repository, RuntimeRoot, ConfigDir, Tooling string
	Installation                                config.PersistentFileSnapshot
	Root                                        ownerInputFile
}

type ownerLegacyContinuation struct {
	Path                                 string
	Before                               config.PersistentFileSnapshot
	Native, Source                       string
	HostBefore                           config.PersistentFileSnapshot
	RegistrationPath                     string
	RegistrationBefore, LegacyHostBefore config.PersistentFileSnapshot
	SourceFacts                          ownerLegacySource
}

// Published releases may add children. Match ordinary source admission: bind
// the directory identity and security metadata, not nlink/size/timestamps.
func ownerLegacyRootFact(root string) (ownerInputFile, error) {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0022 != 0 {
		return ownerInputFile{}, domain.ErrPlanStale
	}
	fact, err := ownerInputIdentity(root, info)
	return ownerInputFile{Exists: fact.Exists, Mode: fact.Mode, Device: fact.Device, Inode: fact.Inode, UID: fact.UID, GID: fact.GID}, err
}

func (s ownerLegacySource) binding() (string, error) {
	raw, err := nativejson.EncodeLosslessNativeJSON(s, 1<<20)
	if err != nil {
		return "", err
	}
	return nativeDigest(raw), nil
}
func (s ownerLegacySource) source(ctx context.Context, cli *CLI, loaded config.Loaded) (string, string, error) {
	if !s.Reference.Valid() || len(s.Tooling) != 64 || strings.Trim(s.Tooling, "0123456789abcdef") != "" || runtimeRootForRepository(s.Repository, s.RuntimeRoot) != s.RuntimeRoot || s.ConfigDir != filepath.Join(s.Repository, "config") {
		return "", "", domain.ErrPlanStale
	}
	root, err := ownerLegacyRootFact(s.RuntimeRoot)
	if err != nil || root != s.Root {
		return "", "", domain.ErrPlanStale
	}
	logical := ownerRepositoryValue(loaded.Context.Paths.ConfigDir, cli.options.RepositoryRoot, s.Repository)
	if filepath.Clean(logical) != s.ConfigDir {
		return "", "", domain.ErrPlanStale
	}
	installation, err := uninstall.ReadInstallationFile(filepath.Join(s.RuntimeRoot, uninstall.ManifestName), loaded.Context.Paths.DataHome, uint32(os.Getuid()))
	if err != nil || installation.ConfigHome != loaded.Context.Paths.ConfigHome || installation.RuntimeRoot != s.RuntimeRoot {
		return "", "", domain.ErrPlanStale
	}
	current, err := config.ReadPersistentFileSnapshot(s.RuntimeRoot, filepath.Join(s.RuntimeRoot, uninstall.ManifestName))
	if err != nil || !reflect.DeepEqual(current, s.Installation) {
		return "", "", domain.ErrPlanStale
	}
	binding, err := s.binding()
	return binding, s.RuntimeRoot, err
}

func captureOwnerLegacyContinuation(c *initLegacyContinuation) (*ownerLegacyContinuation, error) {
	if c == nil {
		return nil, nil
	}
	if c.ownerSource == nil || !c.ownerSource.Reference.Valid() || !c.before.Exists || c.published {
		return nil, errors.New("saved legacy continuation requires original COMPLETE source facts")
	}
	binding, err := c.ownerSource.binding()
	if err != nil || c.source != binding {
		return nil, domain.ErrPlanStale
	}
	grant, err := decodeLegacyContinuation(c.before)
	if err != nil || !reflect.DeepEqual(grant, c.grant) {
		return nil, domain.ErrPlanStale
	}
	return &ownerLegacyContinuation{c.path, c.before, c.native, c.source, c.hostBefore, c.registrationPath, c.registrationBefore, c.legacyHostBefore, *c.ownerSource}, nil
}
func (r *ownerLegacyContinuation) restore() (*initLegacyContinuation, error) {
	if r == nil {
		return nil, nil
	}
	grant, err := decodeLegacyContinuation(r.Before)
	if err != nil {
		return nil, err
	}
	c := &initLegacyContinuation{grant: grant, path: r.Path, before: r.Before, native: r.Native, source: r.Source, hostBefore: r.HostBefore, registrationPath: r.RegistrationPath, registrationBefore: r.RegistrationBefore, legacyHostBefore: r.LegacyHostBefore, ownerSource: &r.SourceFacts}
	canonical, err := captureOwnerLegacyContinuation(c)
	if err != nil || !reflect.DeepEqual(canonical, r) {
		return nil, domain.ErrPlanStale
	}
	return c, nil
}
