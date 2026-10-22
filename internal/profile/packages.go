package profile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/domain"
)

// Package is the native view of one shipped environment package. Missing files
// remain absent; semantic metadata and protected operator files live elsewhere.
type Package struct {
	Name, Root                            string
	Config, Provision, Preset, Descriptor *Source
	Resources                             []Source
	// An unavailable optional preset remains queryable metadata; effects must
	// reject it rather than read or transport the unsafe source.
	PresetError error
}

type FileIdentity struct {
	Mode     uint32 `json:"mode"`
	Device   uint64 `json:"device"`
	Inode    uint64 `json:"inode"`
	Size     int64  `json:"size"`
	Modified int64  `json:"modified"`
	Changed  int64  `json:"changed"`
	Links    uint64 `json:"links"`
	UID      uint32 `json:"uid"`
	GID      uint32 `json:"gid"`
	Digest   string `json:"digest,omitempty"`
}

type Source struct {
	Path     string
	Identity FileIdentity
	parents  map[string]FileIdentity
}

// ProvisionBinding freezes the complete bundle before assessment. It is an
// operation-local native transport, never a persisted settings schema.
type ProvisionBinding struct {
	Root        string                  `json:"root"`
	Entries     map[string]FileIdentity `json:"entries"`
	Environment map[string]string       `json:"environment"`
	Parents     map[string]FileIdentity `json:"parents"`
}

func packageDirectory(root string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(root, "/proc/self/fd/") {
		root = fmt.Sprintf("/proc/%d/fd/%s", os.Getpid(), strings.TrimPrefix(root, "/proc/self/fd/"))
	}
	anchor := strings.TrimSuffix(profileDescriptorRoot.FindString(root+"/"), "/")
	current := string(filepath.Separator)
	remaining := strings.TrimPrefix(root, current)
	if anchor != "" {
		current, remaining = anchor, strings.TrimPrefix(strings.TrimPrefix(root, anchor), "/")
	}
	info, err := os.Stat(current)
	if err != nil || !info.IsDir() {
		return "", errors.New("shipped package directory is unavailable")
	}
	for _, part := range strings.Split(remaining, "/") {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return "", err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("shipped package directory must not traverse symlinks")
		}
	}
	return root, nil
}

func sourceInfoIdentity(info os.FileInfo) (FileIdentity, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (!info.IsDir() && (!info.Mode().IsRegular() || st.Nlink != 1)) {
		return FileIdentity{}, errors.New("shipped source must be a single-link regular file or directory")
	}
	return FileIdentity{Mode: st.Mode, Device: uint64(st.Dev), Inode: st.Ino, Size: st.Size, Modified: st.Mtim.Nano(), Changed: st.Ctim.Nano(), Links: uint64(st.Nlink), UID: st.Uid, GID: st.Gid}, nil
}

func checkBoundSource(file *os.File, expected FileIdentity) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	identity, err := sourceInfoIdentity(info)
	identity.Digest = expected.Digest
	if err != nil || !info.Mode().IsRegular() || identity != expected {
		return errors.New("opened shipped source changed")
	}
	return nil
}

func readBoundSource(file *os.File, expected FileIdentity) ([]byte, error) {
	// Pathname checks cannot authorize a second open. Bind this descriptor,
	// the returned bytes and its final metadata to the captured authority.
	if err := checkBoundSource(file, expected); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, expected.Size+1))
	if err != nil {
		return nil, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(data)) != expected.Digest {
		return nil, errors.New("opened shipped source content changed")
	}
	if err := checkBoundSource(file, expected); err != nil {
		return nil, err
	}
	return data, nil
}

func sourceIdentity(path string, digest bool) (FileIdentity, error) {
	return captureSourceIdentity(path, digest, "")
}

func captureSourceIdentity(path string, digest bool, relative string) (FileIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return FileIdentity{}, err
	}
	identity, err := sourceInfoIdentity(info)
	if err != nil {
		return FileIdentity{}, err
	}
	if (relative == "profile.json" || relative == "integration.json") && identity.Size > 64<<10 {
		return FileIdentity{}, errors.New("shipped declaration must be bounded")
	}
	if relative == "yard.env" && (identity.Mode&0o022 != 0 || identity.Size > 8<<20) {
		return FileIdentity{}, errors.New("profile preset must be bounded and not writable by other users")
	}
	if digest && info.Mode().IsRegular() {
		file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return FileIdentity{}, err
		}
		if err := checkBoundSource(file, identity); err != nil {
			file.Close()
			return FileIdentity{}, errors.New("shipped source changed while opening")
		}
		identity.Digest, err = boundedSourceDigest(file, identity.Size)
		if err == nil {
			err = checkBoundSource(file, identity)
		}
		closeErr := file.Close()
		if err := errors.Join(err, closeErr); err != nil {
			return FileIdentity{}, err
		}
	}
	return identity, nil
}

func boundedSourceDigest(reader io.Reader, size int64) (string, error) {
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(reader, size+1))
	if err != nil {
		return "", err
	}
	if n != size {
		return "", errors.New("shipped source changed while hashing")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (source Source) Read() ([]byte, error) {
	if _, err := packageDirectory(filepath.Dir(source.Path)); err != nil {
		return nil, err
	}
	parents, err := sourceParents(filepath.Dir(source.Path))
	if err != nil || !reflect.DeepEqual(parents, source.parents) {
		return nil, errors.New("shipped source parent changed")
	}
	file, err := os.OpenFile(source.Path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	data, err := readBoundSource(file, source.Identity)
	closeErr := file.Close()
	if err := errors.Join(err, closeErr); err != nil {
		return nil, err
	}
	after, err := sourceIdentity(source.Path, false)
	after.Digest = source.Identity.Digest
	if err != nil || after != source.Identity {
		return nil, errors.New("shipped source changed during read")
	}
	parents, err = sourceParents(filepath.Dir(source.Path))
	if err != nil || !reflect.DeepEqual(parents, source.parents) {
		return nil, errors.New("shipped source parent changed during read")
	}
	return data, nil
}

func sourceParents(directory string) (map[string]FileIdentity, error) {
	parents := map[string]FileIdentity{}
	anchor := strings.TrimSuffix(profileDescriptorRoot.FindString(directory+"/"), "/")
	for current := directory; ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err != nil {
			return nil, err
		}
		st := info.Sys().(*syscall.Stat_t)
		parents[current] = FileIdentity{Mode: st.Mode, Device: uint64(st.Dev), Inode: st.Ino, UID: st.Uid, GID: st.Gid}
		if current == anchor || current == string(filepath.Separator) {
			break
		}
	}
	return parents, nil
}

// PackagesIn shares the native authority with the explicit security test-root
// seam. Production callers supply the shipped config/profiles directory.
func PackagesIn(directory string) ([]Package, error) {
	return packagesIn(repositoryPath(directory))
}

func packagesIn(directory string) ([]Package, error) {
	directory, err := packageDirectory(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	packages := []Package{}
	for _, entry := range entries {
		if !domain.SafeName(entry.Name()) {
			continue
		}
		if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			continue
		}
		pkg, err := resolvePackageIn(directory, entry.Name())
		if err != nil {
			return nil, err
		}
		packages = append(packages, pkg)
	}
	return packages, nil
}

func repositoryPath(root string) string {
	if strings.HasPrefix(root, "/proc/self/fd/") {
		root = fmt.Sprintf("/proc/%d/fd/%s", os.Getpid(), strings.TrimPrefix(root, "/proc/self/fd/"))
	}
	if !pinnedRepositoryRoot.MatchString(root) && profileDescriptorRoot.FindString(root+"/") == "" {
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			return resolved
		}
	}
	return root
}

func Packages(root string) ([]Package, error) {
	return packagesIn(filepath.Join(repositoryPath(root), "config", "profiles"))
}

func ResolvePackageIn(directory, name string) (Package, error) {
	return resolvePackageIn(repositoryPath(directory), name)
}

func resolvePackageIn(directory, name string) (Package, error) {
	if !domain.SafeName(name) {
		return Package{}, fmt.Errorf("invalid environment profile %q", name)
	}
	root, err := packageDirectory(filepath.Join(directory, name))
	if err != nil {
		return Package{}, err
	}
	pkg := Package{Name: name, Root: root}
	for _, ref := range []struct {
		path   string
		target **Source
	}{{"profile.conf", &pkg.Config}, {"provision.sh", &pkg.Provision}, {"profile.json", &pkg.Descriptor}} {
		source, err := packageSource(root, ref.path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return Package{}, err
		}
		*ref.target = &source
	}
	preset, presetErr := packageSource(root, "yard.env")
	if presetErr == nil {
		pkg.Preset = &preset
	} else if !errors.Is(presetErr, os.ErrNotExist) {
		pkg.PresetError = presetErr
	}
	resources := filepath.Join(root, "resources")
	if _, err := packageDirectory(resources); err == nil {
		entries, err := os.ReadDir(resources)
		if err != nil {
			return Package{}, err
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".res") {
				continue
			}
			source, err := packageSource(root, filepath.Join("resources", entry.Name()))
			if err != nil {
				return Package{}, err
			}
			pkg.Resources = append(pkg.Resources, source)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Package{}, err
	}
	return pkg, nil
}

func ResolvePackage(root, name string) (Package, error) {
	return resolvePackageIn(filepath.Join(repositoryPath(root), "config", "profiles"), name)
}

// ResolvePackageInConfig allows an explicit config-root alias while keeping
// appended profile directories inside that selected root.
func ResolvePackageInConfig(configDir, name string) (Package, error) {
	return resolvePackageIn(filepath.Join(repositoryPath(configDir), "profiles"), name)
}

func packageSource(root, relative string) (Source, error) {
	path := filepath.Join(root, relative)
	if _, err := packageDirectory(filepath.Dir(path)); err != nil {
		return Source{}, err
	}
	identity, err := captureSourceIdentity(path, true, relative)
	if err != nil {
		return Source{}, err
	}
	if identity.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return Source{}, fmt.Errorf("unsafe shipped source %s", path)
	}
	parents, err := sourceParents(filepath.Dir(path))
	return Source{Path: path, Identity: identity, parents: parents}, err
}

// CheckProvisionHook checks the hook protocol without authorizing transport of
// the complete package. Inventory can retain metadata for an unavailable bundle.
func (pkg Package) CheckProvisionHook() error {
	if pkg.Provision == nil {
		return errors.New("profile provision hook is missing")
	}
	data, err := pkg.Provision.Read()
	if err != nil {
		return err
	}
	if !strings.Contains(string(data), "\n# subyard-provision-check-v1\n") {
		return fmt.Errorf("profile %q provision hook does not support check protocol", pkg.Name)
	}
	return nil
}

func (pkg Package) BindProvision() (ProvisionBinding, error) {
	if pkg.Provision == nil || pkg.Config == nil {
		return ProvisionBinding{}, errors.New("profile provision hook or config is missing")
	}
	if pkg.PresetError != nil {
		return ProvisionBinding{}, pkg.PresetError
	}
	if err := pkg.CheckProvisionHook(); err != nil {
		return ProvisionBinding{}, err
	}
	entries := map[string]FileIdentity{}
	err := filepath.WalkDir(pkg.Root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(pkg.Root, path)
		if err != nil {
			return err
		}
		identity, err := captureSourceIdentity(path, true, relative)
		if err != nil {
			return err
		}
		entries[relative] = identity
		return nil
	})
	if err != nil {
		return ProvisionBinding{}, err
	}
	for _, source := range []*Source{pkg.Config, pkg.Provision, pkg.Preset, pkg.Descriptor} {
		if source == nil {
			continue
		}
		relative, _ := filepath.Rel(pkg.Root, source.Path)
		if entries[relative] != source.Identity {
			return ProvisionBinding{}, errors.New("profile source changed during binding")
		}
	}
	parents, err := sourceParents(pkg.Root)
	return ProvisionBinding{Root: pkg.Root, Entries: entries, Parents: parents}, err
}

func (binding ProvisionBinding) Check() error {
	pkg, err := resolvePackageIn(filepath.Dir(binding.Root), filepath.Base(binding.Root))
	if err != nil {
		return err
	}
	fresh, err := pkg.BindProvision()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(fresh.Entries, binding.Entries) || !reflect.DeepEqual(fresh.Parents, binding.Parents) {
		return errors.New("profile provision bundle changed")
	}
	return nil
}

type shippedDeclaration struct {
	path string
	data []byte
}

// Both package kinds share one immutable source authority and path boundary.
func loadDeclarations(root, kind, filename string) ([]shippedDeclaration, error) {
	if kind == "profiles" {
		packages, err := Packages(root)
		if err != nil {
			return nil, err
		}
		declarations := []shippedDeclaration{}
		for _, pkg := range packages {
			if pkg.Descriptor == nil {
				continue
			}
			if pkg.Descriptor.Identity.Size > 64<<10 {
				return nil, errors.New("shipped declaration must be bounded")
			}
			data, err := pkg.Descriptor.Read()
			if err != nil {
				return nil, err
			}
			declarations = append(declarations, shippedDeclaration{pkg.Descriptor.Path, data})
		}
		return declarations, nil
	}
	root, err := filepath.Abs(repositoryPath(root))
	if err != nil {
		return nil, err
	}
	paths, err := filepath.Glob(filepath.Join(root, "config", kind, "*", filename))
	if err != nil {
		return nil, err
	}
	result := make([]shippedDeclaration, 0, len(paths))
	for _, path := range paths {
		source, err := packageSource(filepath.Dir(path), filename)
		if err != nil {
			return nil, err
		}
		if source.Identity.Size > 64<<10 {
			return nil, errors.New("shipped declaration must be bounded")
		}
		data, err := source.Read()
		if err != nil {
			return nil, err
		}
		result = append(result, shippedDeclaration{source.Path, data})
	}
	return result, nil
}
