package resourceendpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	SourceOverride = "override"
	SourceSaved    = "saved"
	SourceAuto     = "auto"

	stateSchema  = 1
	maxStateSize = 1 << 20
)

var (
	ErrPlanStale    = errors.New("endpoint plan stale")
	ErrPortReserved = errors.New("endpoint port reserved")
)

type Request struct {
	Directory     string
	Yard          string
	Resource      string
	Host          string
	Port          string
	PreferredPort int
	ReservedPorts []int
}

type Plan struct {
	Host       string
	Port       int
	HostSource string
	PortSource string
	Snapshot   string
}

type Manager struct {
	Discover func(context.Context) (string, error)
	Occupied func(context.Context, string, int) (bool, error)
}

type allocation struct {
	Yard     string `json:"yard"`
	Resource string `json:"resource"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
}

type state struct {
	Schema      int          `json:"schema"`
	Allocations []allocation `json:"allocations"`
}

func (manager Manager) Preview(ctx context.Context, request Request) (Plan, error) {
	if err := validateRequest(request); err != nil {
		return Plan{}, err
	}
	current, err := readState(request.Directory)
	if err != nil {
		return Plan{}, err
	}
	return manager.preview(ctx, request, current)
}

func (manager Manager) preview(ctx context.Context, request Request, current state) (Plan, error) {
	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}
	plan := Plan{Snapshot: stateSnapshot(current)}
	saved, savedExists := findAllocation(current, request.Yard, request.Resource)

	if request.Host != "" {
		plan.Host, plan.HostSource = request.Host, SourceOverride
	} else if savedExists {
		plan.Host, plan.HostSource = saved.Host, SourceSaved
	} else {
		if manager.Discover == nil {
			return Plan{}, errors.New("endpoint host discovery is unavailable")
		}
		host, err := manager.Discover(ctx)
		if err != nil {
			return Plan{}, fmt.Errorf("discover endpoint host: %w", err)
		}
		if !safeHost(host) {
			return Plan{}, errors.New("discovered endpoint host is invalid")
		}
		plan.Host, plan.HostSource = host, SourceAuto
	}

	if request.Port != "" {
		port, err := parsePort(request.Port)
		if err != nil {
			return Plan{}, err
		}
		plan.Port, plan.PortSource = port, SourceOverride
	} else if savedExists {
		plan.Port, plan.PortSource = saved.Port, SourceSaved
	} else {
		plan.PortSource = SourceAuto
		reserved := reservedSet(request.ReservedPorts)
		for _, existing := range current.Allocations {
			reserved[existing.Port] = struct{}{}
		}
		if manager.Occupied == nil {
			return Plan{}, errors.New("endpoint occupancy probe is unavailable")
		}
		for candidate := request.PreferredPort; candidate <= 65535; candidate++ {
			if err := ctx.Err(); err != nil {
				return Plan{}, err
			}
			if _, found := reserved[candidate]; found {
				continue
			}
			occupied, err := manager.Occupied(ctx, plan.Host, candidate)
			if err != nil {
				return Plan{}, fmt.Errorf("probe endpoint port %d: %w", candidate, err)
			}
			if !occupied {
				plan.Port = candidate
				break
			}
		}
		if plan.Port == 0 {
			return Plan{}, errors.New("no endpoint port is available")
		}
	}

	if conflict, found := portOwner(current, plan.Port, request.Yard, request.Resource); found {
		return Plan{}, fmt.Errorf("%w: port %d belongs to %s/%s",
			ErrPortReserved, plan.Port, conflict.Yard, conflict.Resource)
	}
	if _, found := reservedSet(request.ReservedPorts)[plan.Port]; found {
		return Plan{}, fmt.Errorf("%w: port %d is reserved by owner configuration", ErrPortReserved, plan.Port)
	}
	return plan, nil
}

func (manager Manager) Commit(ctx context.Context, request Request, approved Plan) error {
	return manager.CommitWith(ctx, request, approved, nil)
}

// CommitWith revalidates an approved endpoint under the owner-wide allocation
// lock, then runs beforePublish before persisting the endpoint. Callers can use
// the callback to make a related compare-and-swap update without publishing an
// endpoint after that update has already become stale.
func (manager Manager) CommitWith(
	ctx context.Context,
	request Request,
	approved Plan,
	beforePublish func() error,
) error {
	if err := validateRequest(request); err != nil {
		return err
	}
	if err := validatePlan(approved); err != nil {
		return err
	}
	if request.Host != "" && (approved.Host != request.Host || approved.HostSource != SourceOverride) {
		return errors.New("approved endpoint plan does not match host override")
	}
	if request.Port != "" {
		port, _ := parsePort(request.Port)
		if approved.Port != port || approved.PortSource != SourceOverride {
			return errors.New("approved endpoint plan does not match port override")
		}
	}
	if err := ensureStateDirectory(request.Directory); err != nil {
		return err
	}
	lock, err := acquireLock(ctx, request.Directory)
	if err != nil {
		return err
	}
	defer releaseLock(lock)
	current, err := readState(request.Directory)
	if err != nil {
		return err
	}
	if saved, exists := findAllocation(current, request.Yard, request.Resource); exists &&
		saved.Host == approved.Host && saved.Port == approved.Port {
		if beforePublish != nil {
			return beforePublish()
		}
		return nil
	}
	revalidated, err := manager.preview(ctx, request, current)
	if err != nil {
		if errors.Is(err, ErrPortReserved) {
			return fmt.Errorf("%w: %v", ErrPlanStale, err)
		}
		return err
	}
	if revalidated != approved {
		return ErrPlanStale
	}
	if beforePublish != nil {
		if err := beforePublish(); err != nil {
			return err
		}
	}
	updated := false
	for index := range current.Allocations {
		if current.Allocations[index].Yard == request.Yard && current.Allocations[index].Resource == request.Resource {
			current.Allocations[index] = allocation{Yard: request.Yard, Resource: request.Resource, Host: approved.Host, Port: approved.Port}
			updated = true
			break
		}
	}
	if !updated {
		current.Allocations = append(current.Allocations, allocation{
			Yard: request.Yard, Resource: request.Resource, Host: approved.Host, Port: approved.Port,
		})
	}
	return writeState(request.Directory, current)
}

func ReadSaved(directory, yard, resource string) (string, int, bool, error) {
	request := Request{Directory: directory, Yard: yard, Resource: resource, PreferredPort: 1}
	if err := validateRequest(request); err != nil {
		return "", 0, false, err
	}
	current, err := readState(directory)
	if err != nil {
		return "", 0, false, err
	}
	saved, exists := findAllocation(current, yard, resource)
	if !exists {
		return "", 0, false, nil
	}
	return saved.Host, saved.Port, true, nil
}

func validateRequest(request Request) error {
	if !filepath.IsAbs(request.Directory) || filepath.Clean(request.Directory) != request.Directory {
		return errors.New("endpoint state directory must be an absolute normalized path")
	}
	if !safeIdentity(request.Yard) || !safeIdentity(request.Resource) {
		return errors.New("endpoint yard and resource identities are invalid")
	}
	if request.Host != "" && !safeHost(request.Host) {
		return errors.New("endpoint host override is invalid")
	}
	if request.Port != "" {
		if _, err := parsePort(request.Port); err != nil {
			return err
		}
	}
	if request.PreferredPort < 1 || request.PreferredPort > 65535 {
		return errors.New("preferred endpoint port must be in range 1..65535")
	}
	for _, port := range request.ReservedPorts {
		if port < 1 || port > 65535 {
			return errors.New("reserved endpoint port must be in range 1..65535")
		}
	}
	return nil
}

func validatePlan(plan Plan) error {
	if !safeHost(plan.Host) || plan.Port < 1 || plan.Port > 65535 || plan.Snapshot == "" {
		return errors.New("approved endpoint plan is invalid")
	}
	if !validSource(plan.HostSource) || !validSource(plan.PortSource) {
		return errors.New("approved endpoint plan has invalid sources")
	}
	return nil
}

func validSource(source string) bool {
	return source == SourceOverride || source == SourceSaved || source == SourceAuto
}

func parsePort(value string) (int, error) {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != value {
		return 0, errors.New("endpoint port override must be a canonical integer in range 1..65535")
	}
	return port, nil
}

func safeIdentity(value string) bool {
	if value == "" || len(value) > 255 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("._-", character) {
			continue
		}
		return false
	}
	return value[0] != '-' && value[len(value)-1] != '-'
}

func safeHost(value string) bool {
	if ip := net.ParseIP(value); ip != nil {
		return ip.To4() != nil && !ip.IsUnspecified() && !ip.IsMulticast()
	}
	if value == "" || len(value) > 253 || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '.' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func reservedSet(ports []int) map[int]struct{} {
	result := make(map[int]struct{}, len(ports))
	for _, port := range ports {
		result[port] = struct{}{}
	}
	return result
}

func findAllocation(current state, yard, resource string) (allocation, bool) {
	for _, existing := range current.Allocations {
		if existing.Yard == yard && existing.Resource == resource {
			return existing, true
		}
	}
	return allocation{}, false
}

func portOwner(current state, port int, yard, resource string) (allocation, bool) {
	for _, existing := range current.Allocations {
		if existing.Port == port && (existing.Yard != yard || existing.Resource != resource) {
			return existing, true
		}
	}
	return allocation{}, false
}

func emptyState() state { return state{Schema: stateSchema, Allocations: []allocation{}} }

func stateSnapshot(current state) string {
	payload, _ := json.Marshal(canonicalState(current))
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func canonicalState(current state) state {
	clone := state{Schema: stateSchema, Allocations: append([]allocation(nil), current.Allocations...)}
	sort.Slice(clone.Allocations, func(left, right int) bool {
		if clone.Allocations[left].Yard != clone.Allocations[right].Yard {
			return clone.Allocations[left].Yard < clone.Allocations[right].Yard
		}
		return clone.Allocations[left].Resource < clone.Allocations[right].Resource
	})
	return clone
}

func readState(directory string) (state, error) {
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return emptyState(), nil
	}
	if err != nil {
		return state{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || !ownedByUser(info) {
		return state{}, errors.New("endpoint state directory has unsafe type or permissions")
	}
	path := filepath.Join(directory, "state.json")
	info, err = os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return emptyState(), nil
	}
	if err != nil {
		return state{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || info.Size() > maxStateSize {
		return state{}, errors.New("endpoint state file has unsafe type, permissions, or size")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return state{}, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !safeOwnedFile(opened) || !os.SameFile(info, opened) {
		return state{}, errors.New("endpoint state file changed or has unsafe ownership")
	}
	decoder := json.NewDecoder(io.LimitReader(file, maxStateSize+1))
	decoder.DisallowUnknownFields()
	var current state
	if err := decoder.Decode(&current); err != nil {
		return state{}, fmt.Errorf("decode endpoint state: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return state{}, errors.New("endpoint state contains trailing data")
	}
	if err := validateState(current); err != nil {
		return state{}, err
	}
	return canonicalState(current), nil
}

func validateState(current state) error {
	if current.Schema != stateSchema || current.Allocations == nil || len(current.Allocations) > 4096 {
		return errors.New("endpoint state has an unsupported schema or size")
	}
	identities := make(map[string]struct{}, len(current.Allocations))
	ports := make(map[int]struct{}, len(current.Allocations))
	for _, existing := range current.Allocations {
		if !safeIdentity(existing.Yard) || !safeIdentity(existing.Resource) || !safeHost(existing.Host) ||
			existing.Port < 1 || existing.Port > 65535 {
			return errors.New("endpoint state contains an invalid allocation")
		}
		identity := existing.Yard + "\x00" + existing.Resource
		if _, duplicate := identities[identity]; duplicate {
			return errors.New("endpoint state contains a duplicate allocation")
		}
		identities[identity] = struct{}{}
		if _, duplicate := ports[existing.Port]; duplicate {
			return errors.New("endpoint state contains a duplicate port reservation")
		}
		ports[existing.Port] = struct{}{}
	}
	return nil
}

func ensureStateDirectory(directory string) error {
	parent := filepath.Dir(directory)
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 || !ownedByUser(info) {
		if err != nil {
			return fmt.Errorf("inspect endpoint state parent: %w", err)
		}
		return fmt.Errorf("endpoint state parent has unsafe type or permissions: %o", info.Mode().Perm())
	}
	created := false
	err = os.Mkdir(directory, 0o700)
	if err == nil {
		created = true
	}
	if err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err = os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || !ownedByUser(info) {
		return errors.New("endpoint state directory has unsafe type or permissions")
	}
	if created {
		parentDirectory, err := os.Open(parent)
		if err != nil {
			return err
		}
		if err := parentDirectory.Sync(); err != nil {
			parentDirectory.Close()
			return err
		}
		if err := parentDirectory.Close(); err != nil {
			return err
		}
	}
	return nil
}

func acquireLock(ctx context.Context, directory string) (*os.File, error) {
	lock, err := os.OpenFile(filepath.Join(directory, ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := lock.Stat()
	if err != nil || !safeOwnedFile(info) {
		lock.Close()
		return nil, errors.New("endpoint state lock has unsafe type or permissions")
	}
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			lock.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			lock.Close()
			return nil, context.Cause(ctx)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func ownedByUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid())
}

func safeOwnedFile(info os.FileInfo) bool {
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || !ownedByUser(info) {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}

func releaseLock(lock *os.File) {
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	_ = lock.Close()
}

func writeState(directory string, current state) error {
	current = canonicalState(current)
	if err := validateState(current); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".state.json.tmp-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	published := false
	defer func() {
		_ = temporary.Close()
		if !published {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(current); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, filepath.Join(directory, "state.json")); err != nil {
		return err
	}
	published = true
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
