// Package ownerservice stores infrastructure runtime bindings for one owner.
// Callers hold the ordinary owner mutation barrier while publishing or removing.
package ownerservice

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/domain"
)

const SchemaVersion = 1

// Public is the complete agent-visible projection. It carries no configuration,
// filesystem roots, owner transport, controller identity or lease capabilities.
type Public struct {
	SchemaVersion int    `json:"schema_version"`
	Owner         string `json:"owner"`
	Service       string `json:"service"`
	Pool          string `json:"pool"`
	Yard          string `json:"yard"`
	Project       string `json:"project"`
	Instance      string `json:"instance"`
	UUID          string `json:"runtime_uuid"`
	Generation    string `json:"registration_generation"`
}

type Registration struct {
	Public
	Ownership string `json:"ownership"`
}

type Registry struct{ Root string }

type ResolutionError struct {
	Code       string   `json:"code"`
	Owner      string   `json:"owner"`
	Service    string   `json:"service"`
	Pool       string   `json:"pool,omitempty"`
	Candidates []Public `json:"candidates,omitempty"`
}

func (failure *ResolutionError) Error() string {
	message := fmt.Sprintf("%s: owner=%s service=%s pool=%s", failure.Code, failure.Owner, failure.Service, failure.Pool)
	switch failure.Code {
	case "service_absent":
		return message + "; install or register the service on this owner"
	case "service_ambiguous":
		return message + "; select an exact --pool"
	case "service_owner_changed":
		if len(failure.Candidates) > 0 {
			record := failure.Candidates[0]
			return message + fmt.Sprintf("; verify ownership and run yard service register %s --yard %s --pool %s", record.Service, record.Yard, record.Pool)
		}
		return message + "; explicitly re-register the verified owned runtime"
	default:
		return message + "; runtime or trusted publication is unavailable"
	}
}

func (record Public) Validate() error {
	if record.SchemaVersion != SchemaVersion || !domain.SafeID(record.Owner) || strings.ContainsAny(record.Owner, `/\\`) || !domain.SafeName(record.Service) || !domain.SafeName(record.Pool) || !domain.SafeName(record.Yard) || !domain.SafeName(record.Project) || !domain.SafeName(record.Instance) || !domain.SafeID(record.UUID) || len(record.UUID) > 128 || !digest(record.Generation) {
		return errors.New("invalid infrastructure service registration")
	}
	return nil
}

func digest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && strings.ToLower(value) == value
}

func NewGeneration() (string, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(random[:]), nil
}

func (registry Registry) List(service string) ([]Registration, error) {
	if !domain.SafeName(service) {
		return nil, errors.New("invalid infrastructure service ID")
	}
	if err := directory(registry.Root, false, 0o700); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	root := filepath.Join(registry.Root, service)
	if err := directory(root, false, 0o700); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var records []Registration
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".pending-") {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".json") || entry.IsDir() {
			return nil, errors.New("unsafe service registry entry")
		}
		var record Registration
		if err := read(filepath.Join(root, entry.Name()), &record, 0o600); err != nil {
			return nil, err
		}
		if err := record.Validate(); err != nil {
			return nil, err
		}
		if !digest(record.Ownership) || record.Service != service || entry.Name() != record.Pool+".json" {
			return nil, errors.New("inconsistent service registry entry")
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Pool < records[j].Pool })
	return records, nil
}

func (registry Registry) Resolve(owner, service, pool string) (Registration, error) {
	records, err := registry.List(service)
	if err != nil {
		return Registration{}, err
	}
	failure := &ResolutionError{Code: "service_absent", Owner: owner, Service: service, Pool: pool}
	for _, record := range records {
		if pool != "" && record.Pool != pool {
			continue
		}
		if record.Owner != owner {
			return Registration{}, &ResolutionError{Code: "service_owner_changed", Owner: owner, Service: service, Pool: record.Pool, Candidates: []Public{record.Public}}
		}
		failure.Candidates = append(failure.Candidates, record.Public)
		if record.Pool == pool {
			return record, nil
		}
	}
	if pool == "" && len(records) == 1 {
		return records[0], nil
	}
	if pool == "" && len(records) > 1 {
		failure.Code = "service_ambiguous"
	}
	return Registration{}, failure
}

// RepairOwner is an explicit metadata CAS after native installation/UUID proof.
// It cannot change the service, pool or runtime being registered.
func (registry Registry) RepairOwner(before, after Registration) error {
	if err := before.Validate(); err != nil {
		return err
	}
	if err := after.Validate(); err != nil {
		return err
	}
	expected := before
	expected.Owner = after.Owner
	expected.Generation = after.Generation
	if expected != after || before.Owner == after.Owner || before.Generation == after.Generation {
		return domain.ErrPlanStale
	}
	records, err := registry.List(before.Service)
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.Pool == before.Pool {
			if record != before {
				return domain.ErrPlanStale
			}
			return atomic(filepath.Join(registry.Root, before.Service, before.Pool+".json"), after, 0o600)
		}
	}
	return domain.ErrPlanStale
}

// Save is idempotent for an exact registration. Different bindings require an
// explicit removal; a same-name resource can never replace an owned runtime.
func (registry Registry) Save(record Registration) error {
	if err := record.Validate(); err != nil {
		return err
	}
	if !digest(record.Ownership) {
		return errors.New("invalid service ownership binding")
	}
	if err := directory(registry.Root, true, 0o700); err != nil {
		return err
	}
	root := filepath.Join(registry.Root, record.Service)
	if err := directory(root, true, 0o700); err != nil {
		return err
	}
	path := filepath.Join(root, record.Pool+".json")
	var before Registration
	if err := read(path, &before, 0o600); err == nil {
		if before != record {
			return domain.ErrPlanStale
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return atomic(path, record, 0o600)
}

// Publish repairs an interrupted publication of the same private registration.
func (registry Registry) Publish(record Registration, publicationRoot string) error {
	actual, err := registry.Resolve(record.Owner, record.Service, record.Pool)
	if err != nil || actual != record {
		return domain.ErrPlanStale
	}
	for _, path := range []string{publicationRoot, filepath.Join(publicationRoot, record.Service)} {
		if err := directory(path, true, 0o755); err != nil {
			return err
		}
	}
	path := filepath.Join(publicationRoot, record.Service, record.Pool+".json")
	var before Public
	if err := read(path, &before, 0o644); err == nil {
		if err := before.Validate(); err != nil {
			return err
		}
		before.Owner, before.Generation = record.Owner, record.Generation
		if before != record.Public {
			return domain.ErrPlanStale
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return atomic(path, record.Public, 0o644)
}

func (registry Registry) Published(record Registration, publicationRoot string) error {
	for _, path := range []string{publicationRoot, filepath.Join(publicationRoot, record.Service)} {
		if err := directory(path, false, 0o755); err != nil {
			return err
		}
	}
	var public Public
	if err := read(filepath.Join(publicationRoot, record.Service, record.Pool+".json"), &public, 0o644); err != nil {
		return err
	}
	if public != record.Public {
		return domain.ErrPlanStale
	}
	return nil
}

func (registry Registry) Remove(record Registration, publicationRoot string) error {
	actual, err := registry.Resolve(record.Owner, record.Service, record.Pool)
	if err != nil || actual != record {
		return domain.ErrPlanStale
	}
	path := filepath.Join(publicationRoot, record.Service, record.Pool+".json")
	if err := registry.Published(record, publicationRoot); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Remove(filepath.Join(registry.Root, record.Service, record.Pool+".json"))
}

func directory(path string, create bool, mode os.FileMode) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("noncanonical service registry directory")
	}
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("unsafe service registry ancestor")
		}
		if parent == "/" {
			break
		}
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && create {
		if err := os.Mkdir(path, mode); err != nil {
			return err
		}
		if err := os.Chmod(path, mode); err != nil {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != mode || stat.Uid != uint32(os.Getuid()) {
		return errors.New("unsafe service registry directory")
	}
	return nil
}

func read(path string, value any, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != mode || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 || info.Size() > 16384 {
		return errors.New("unsafe service registry file")
	}
	payload, err := io.ReadAll(io.LimitReader(file, 16385))
	if err != nil || len(payload) > 16384 {
		return errors.New("service registry read failed")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("trailing service registry data")
	}
	return nil
}

func atomic(path string, value any, mode os.FileMode) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".pending-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(append(payload, '\n')); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}
