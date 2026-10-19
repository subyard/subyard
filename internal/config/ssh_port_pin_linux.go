package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Subyard/Subyard/internal/domain"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// SSHPortPin is the dev pilot's closed component recipe, not a JournalV2 record.
// It retains semantic pin and byte/identity witnesses, never raw config content.
type SSHPortPin struct {
	Version      int                          `json:"version"`
	Yard         string                       `json:"yard"`
	Target       string                       `json:"target"`
	Port         int                          `json:"port"`
	BeforeAbsent bool                         `json:"beforeAbsent"`
	Expected     PersistentBinding            `json:"expected"`
	Desired      string                       `json:"desired"`
	Root         PersistentFileIdentity       `json:"root"`
	Files        []PersistentPrecondition     `json:"files"`
	Directories  []PersistentDirectoryBinding `json:"directories"`
}

const SSHPortPinMarker = "subyard-release-owner-pilot-v1\n"

func pinPath(yard, target string) (string, error) {
	if !domain.SafeID(yard) || len(yard) > 50 {
		return "", errors.New("invalid pin yard")
	}
	switch target {
	case "canonical":
		return "yards/" + yard + "/config.env", nil
	case "flat":
		return "yards/" + yard + ".env", nil
	}
	return "", errors.New("invalid pin target class")
}
func pinExcluded(path string) []string {
	return []string{".owner", path, filepath.ToSlash(filepath.Join(filepath.Dir(path), ".config.env.release-transition.pending")), filepath.ToSlash(filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".release-transition.pending"))}
}
func pinInputs(root *os.File, path string) ([]PersistentPrecondition, []PersistentDirectoryBinding, error) {
	files := []PersistentPrecondition{}
	dirs := []PersistentDirectoryBinding{}
	excluded := pinExcluded(path)
	count, total := 0, 0
	var walk func(string) error
	walk = func(relative string) error {
		d, err := ReadPersistentDirectoryAt(root, relative)
		if err != nil {
			return err
		}
		d.Entries = slices.DeleteFunc(d.Entries, func(name string) bool {
			return slices.Contains(excluded, filepath.ToSlash(filepath.Join(relative, name)))
		})
		dirs = append(dirs, d)
		for _, name := range d.Entries {
			child := filepath.ToSlash(filepath.Join(relative, name))
			if !domain.SafeID(name) || len(name) > 128 || len(child) > 512 || strings.Count(child, "/") > 16 {
				return errors.New("pin fixture exceeds path bound")
			}
			count++
			if count > 2048 {
				return errors.New("pin fixture exceeds entry bound")
			}
			if _, err := ReadPersistentDirectoryAt(root, child); err == nil {
				if err = walk(child); err != nil {
					return err
				}
				continue
			}
			snap, err := ReadPersistentSnapshotAt(root, child)
			if err != nil || !snap.Exists {
				return errors.New("unsafe pin source")
			}
			total += len(snap.Content)
			if total > 16<<20 {
				return errors.New("pin fixture exceeds content bound")
			}
			files = append(files, PersistentPrecondition{child, BindPersistentSnapshot(snap)})
		}
		return nil
	}
	err := walk(".")
	return files, dirs, err
}
func pinFixture(root *os.File) error {
	s, err := ReadPersistentSnapshotAt(root, "owner.marker")
	if err != nil || !s.Exists || string(s.Content) != SSHPortPinMarker {
		return errors.New("not a marked pilot fixture")
	}
	return nil
}
func pinSame(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
func PrepareSSHPortPin(root *os.File, yard string) (SSHPortPin, error) {
	if err := pinFixture(root); err != nil {
		return SSHPortPin{}, err
	}
	path, err := pinPath(yard, "canonical")
	if err != nil {
		return SSHPortPin{}, err
	}
	canonical, err := ReadPersistentSnapshotAt(root, path)
	if err != nil {
		return SSHPortPin{}, err
	}
	flatPath, _ := pinPath(yard, "flat")
	flat, err := ReadPersistentSnapshotAt(root, flatPath)
	if err != nil || canonical.Exists && flat.Exists {
		return SSHPortPin{}, errors.New("ambiguous pin target")
	}
	target := "canonical"
	if flat.Exists {
		target = "flat"
		path = flatPath
		canonical = flat
	}
	// Require existing parent: authorized file creation cannot change the captured
	// directory topology. Wider creation grammars remain with the production owner.
	if _, err = ReadPersistentDirectoryAt(root, filepath.ToSlash(filepath.Dir(path))); err != nil {
		return SSHPortPin{}, err
	}
	files, dirs, err := pinInputs(root, path)
	if err != nil {
		return SSHPortPin{}, err
	}
	// Plan from a private authenticated snapshot, never from live path reads.
	// The original root FD remains the only native precondition/CAS authority.
	prefix, cleanup, err := stagePinInputs(root, path, canonical, files, dirs)
	if err != nil {
		return SSHPortPin{}, err
	}
	defer cleanup()
	repository := prefix + "/repository"
	names, err := YardNames(repository+"/config", prefix)
	if err != nil {
		return SSHPortPin{}, err
	}
	ports := map[int]string{}
	var selected Loaded
	for _, name := range append(names, "default") {
		loaded, e := Load(LoadOptions{RepositoryRoot: repository, OperatorHome: prefix, YardName: name, DisablePrivate: true, ConfigLocked: true, Environment: map[string]string{"SUBYARD_CONFIG_HOME": prefix, "SUBYARD_CONFIG_DIR": repository + "/config", "SUBYARD_HOME": prefix + "/data"}})
		if e != nil {
			return SSHPortPin{}, errors.New("pin configuration is ineligible")
		}
		if loaded.Context.AccessKind != domain.AccessLocal {
			continue
		}
		port := loaded.Context.SSHPort
		if previous, ok := ports[port]; ok && previous != name && port != 0 {
			return SSHPortPin{}, errors.New("duplicate pin port")
		}
		ports[port] = name
		if name == yard {
			selected = loaded
		}
	}
	_, _, group := ComponentSelectionSource(selected.Settings["SSH_PORT"])
	if selected.Context.YardName == "" || group != "Git settings" {
		return SSHPortPin{}, errors.New("pin requires effective persistent Git SSH_PORT")
	}
	assignments, err := ParsePersistentAssignments(path, canonical.Content)
	if err != nil {
		return SSHPortPin{}, err
	}
	for _, a := range assignments {
		if a.Name == "SSH_PORT" {
			return SSHPortPin{}, errors.New("local pin already has an assignment")
		}
	}
	value := strconv.Itoa(selected.Context.SSHPort)
	desired, err := EditPersistentAssignmentContent(path, canonical.Content, "SSH_PORT", &value)
	if err != nil {
		return SSHPortPin{}, err
	}
	binding := BindPersistentSnapshot(PersistentFileSnapshot{Exists: true, Content: desired})
	finalFiles, finalDirs, err := pinInputs(root, path)
	if err != nil || !pinSame(files, finalFiles) || !pinSame(dirs, finalDirs) {
		return SSHPortPin{}, ErrPersistentTargetStale
	}
	current, err := ReadPersistentSnapshotAt(root, path)
	if err != nil || BindPersistentSnapshot(current) != BindPersistentSnapshot(canonical) {
		return SSHPortPin{}, ErrPersistentTargetStale
	}
	return SSHPortPin{1, yard, target, selected.Context.SSHPort, true, BindPersistentSnapshot(canonical), binding.Digest, dirs[0].Identity, files, dirs}, nil
}

// ObserveSSHPortPin classifies the exact recipe against native bytes after any
// lost reply. A desired result never requests another CAS.
func ObserveSSHPortPin(root *os.File, p SSHPortPin) (string, error) {
	if err := pinFixture(root); err != nil {
		return "", err
	}
	path, err := pinPath(p.Yard, p.Target)
	if err != nil || p.Version != 1 || !p.BeforeAbsent || p.Port < 0 || p.Port > 65535 {
		return "", errors.New("invalid pin recipe")
	}
	files, dirs, err := pinInputs(root, path)
	if err != nil {
		return "", err
	}
	if len(dirs) == 0 || dirs[0].Identity != p.Root || !pinSame(files, p.Files) || !pinSame(dirs, p.Directories) {
		return "conflict", nil
	}
	snap, err := ReadPersistentSnapshotAt(root, path)
	if err != nil {
		return "", err
	}
	binding := BindPersistentSnapshot(snap)
	if binding == p.Expected {
		return "expected", nil
	}
	mode := uint32(0o600)
	if p.Expected.Exists {
		mode = p.Expected.Identity.Mode & 0o777
	}
	if snap.Exists && binding.Digest == p.Desired && binding.Identity.Mode&0o777 == mode {
		return "desired", nil
	}
	return "conflict", nil
}
func ApplySSHPortPin(root *os.File, p SSHPortPin) (string, error) {
	status, err := ObserveSSHPortPin(root, p)
	if err != nil || status != "expected" {
		return status, err
	}
	path, _ := pinPath(p.Yard, p.Target)
	before, err := ReadPersistentSnapshotAt(root, path)
	if err != nil {
		return "", err
	}
	value := strconv.Itoa(p.Port)
	desired, err := EditPersistentAssignmentContent(path, before.Content, "SSH_PORT", &value)
	if err != nil {
		return "", err
	}
	if BindPersistentSnapshot(PersistentFileSnapshot{Exists: true, Content: desired}).Digest != p.Desired {
		return "", errors.New("pin desired binding mismatch")
	}
	if err = CompareAndSwapLocalSettingsAt(root, path, p.Expected, desired, p.Files, p.Directories, pinExcluded(path)); err != nil {
		return "", err
	}
	return ObserveSSHPortPin(root, p)
}

func stagePinInputs(root *os.File, target string, before PersistentFileSnapshot, files []PersistentPrecondition, dirs []PersistentDirectoryBinding) (string, func(), error) {
	fd, err := openPersistentDirectoryAt(int(root.Fd()), ".owner")
	if err != nil {
		return "", nil, err
	}
	parent := os.NewFile(uintptr(fd), "pilot-store")
	stage, err := os.MkdirTemp(fmt.Sprintf("/proc/self/fd/%d", parent.Fd()), "planning-")
	if err != nil {
		parent.Close()
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(stage); _ = parent.Close() }
	staged, err := os.OpenRoot(stage)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	fail := func(e error) (string, func(), error) { staged.Close(); cleanup(); return "", nil, e }
	if err = staged.Chmod(".", 0o700); err != nil {
		return fail(err)
	}
	for _, d := range dirs {
		if d.Relative == "." {
			continue
		}
		if err = staged.MkdirAll(d.Relative, 0o700); err != nil {
			return fail(err)
		}
		if err = staged.Chmod(d.Relative, 0o700); err != nil {
			return fail(err)
		}
	}
	write := func(relative string, snapshot PersistentFileSnapshot) error {
		if !snapshot.Exists {
			return nil
		}
		if err := staged.WriteFile(relative, snapshot.Content, 0o600); err != nil {
			return err
		}
		return staged.Chmod(relative, os.FileMode(0o600|snapshot.Identity.Mode&0o111))
	}
	for _, input := range files {
		snapshot, e := ReadPersistentSnapshotAt(root, input.Relative)
		if e != nil || BindPersistentSnapshot(snapshot) != input.Binding {
			return fail(ErrPersistentTargetStale)
		}
		if e = write(input.Relative, snapshot); e != nil {
			return fail(e)
		}
	}
	if err = write(target, before); err != nil {
		return fail(err)
	}
	if err = staged.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return stage, cleanup, nil
}
