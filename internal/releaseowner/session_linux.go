package releaseowner

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/uninstall"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
	"golang.org/x/sys/unix"
)

type session struct {
	ownerOrigins           map[string]*invocationObserver
	ownerCompleted         *Binding
	ownerCompletedTerminal string
	ownerResultsPayload    string
	ownerResultsRecipe     string
	ownerResultsSource     string
	image                  string
	effectContext          context.Context
	factory                Factory
	context                *Context
	root                   *os.File
	repository             *os.File
	components             Components
	leases                 []*os.File
	binding                *Binding
	ownerRelease           func()
	installedTransfer      *installedRecipeTransfer
	installedRead          *installedRecipeTransfer
}

func (s *session) releaseLeases() {
	if s.ownerRelease != nil {
		s.ownerRelease()
		s.ownerRelease = nil
	}
	closeFiles(s.leases)
	s.leases = nil
	s.binding = nil
}

func (s *session) close() {
	if s.components != nil {
		s.components.Close()
	}
	s.releaseLeases()
	if s.root != nil {
		s.root.Close()
	}
	if s.repository != nil {
		s.repository.Close()
	}
}
func fingerprint(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, e := hex.DecodeString(s)
	return e == nil && strings.ToLower(s) == s
}
func safePath(s string) bool {
	return filepath.IsAbs(s) && filepath.Clean(s) == s && s != "/" && !strings.ContainsAny(s, "\x00\r\n")
}
func bindingValid(b *Binding) bool {
	return b != nil && fingerprint(b.Session) && len(b.Transaction) <= 128 && domain.SafeID(b.Transaction) && strings.HasPrefix(b.Plan, "plan-v1-") && fingerprint(strings.TrimPrefix(b.Plan, "plan-v1-")) && fingerprint(b.Journal) && fingerprint(b.Artifact) && fingerprint(b.Helper) && b.Ordinal > 0 && b.Ordinal <= 4096
}
func retain(file *os.File) (*os.File, error) {
	fd, e := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	return os.NewFile(uintptr(fd), "retained release capability"), nil
}
func regular(id Identity) bool {
	return id.Mode&unix.S_IFMT == unix.S_IFREG && id.UID == uint32(os.Getuid()) && id.Mode&0o777 == 0o600 && id.Links == 1
}
func (s *session) handle(ctx context.Context, raw []byte, files []*os.File) (Reply, error) {
	var request Request
	if err := strict(raw, &request); err != nil {
		return Reply{}, err
	}
	allowed := map[string][]string{
		"capture-ingress": {"session", "bindingIngress", "legacyRust"}, "observe-ingress": {"session", "step"}, "verify-ingress": {"session", "step"}, "apply-ingress": {"binding", "step"},
		"observe-legacy-checkpoint": {"session", "originalV1"},
		"initialize":                {"context"}, "capture-inventory": {"session"}, "capture-yard": {"session", "yard", "resolved"}, "prepare-settings": {"session", "yard", "edits"}, "capture-owner": {"session"}, "capture-activation": {"session", "owner", "fresh"},
		"claim-lease": {"binding", "update", "owner"}, "observe-settings": {"session", "recipe"}, "capture-settings": {"session", "recipe"}, "project-settings": {"session", "recipe", "edits"}, "apply-settings": {"binding", "recipe"}, "apply-owner": {"binding", "before", "token"}, "cleanup-owner": {"binding", "before", "token"}, "query-owner-cleanup": {"binding", "before", "token"}, "apply-activation": {"binding", "target"}, "query-invocation": {"binding"}, "release-lease": {"session"},
		"fence-activation":         {"binding", "target", "successor"},
		"capture-config-layout":    {"session"},
		"observe-owner":            {"session", "before", "token"},
		"project-activation":       {"session", "before"},
		"capture-rollback-support": {"session", "rollbackRepositories", "targetOwners"},
		"read-installed-recipe":    {"session", "recipeHash", "offset"},
		"bind-installed-recipe":    {"session", "recipeHash", "offset", "total", "content", "target"},
		"bind-owner-results":       {"session", "binding", "producer", "before", "token"},
		"resolve-owner-target":     {"session", "binding", "target"},
		"resolve-after-owner":      {"session", "binding", "target"},
	}
	keys, ok := allowed[request.Op]
	if !ok {
		return Reply{}, errors.New("unknown native verb")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return Reply{}, errors.New("native fields")
	}
	for field := range fields {
		if field == "op" {
			continue
		}
		found := false
		for _, key := range keys {
			found = found || field == key
		}
		if !found {
			return Reply{}, errors.New("unexpected native field")
		}
	}
	if request.Op == "initialize" {
		if s.context != nil || len(files) != 2 || request.Context == nil {
			return Reply{}, errors.New("native initialization")
		}
		c := request.Context
		got, e := IdentityOf(files[0])
		if e != nil || got != c.Root || got.Mode&unix.S_IFMT != unix.S_IFDIR || got.UID != uint32(os.Getuid()) || got.Mode&0o022 != 0 || !fingerprint(c.Session) || !fingerprint(c.Artifact) || !domain.SafeName(c.Yard) || !domain.SafeID(c.Target) || !safePath(c.DataHome) || !safePath(c.RepositoryRoot) || !safePath(c.ConfigHome) || !safePath(c.RuntimeRoot) {
			return Reply{}, errors.New("native context")
		}
		root, e := retain(files[0])
		if e != nil {
			return Reply{}, e
		}
		repositoryID, e := IdentityOf(files[1])
		if e != nil || repositoryID != c.Repository || repositoryID.Mode&unix.S_IFMT != unix.S_IFDIR || repositoryID.Mode&0o022 != 0 || (repositoryID.UID != uint32(os.Getuid()) && repositoryID.UID != 0) {
			root.Close()
			return Reply{}, errors.New("native repository root identity")
		}
		repository, e := retain(files[1])
		if e != nil {
			root.Close()
			return Reply{}, e
		}
		c.PhysicalRepositoryRoot = fmt.Sprintf("/proc/self/fd/%d", repository.Fd())
		components, e := s.factory(ctx, *c, root)
		if e != nil {
			root.Close()
			repository.Close()
			return Reply{Code: "unavailable"}, nil
		}
		s.root = root
		s.repository = repository
		s.components = components
		s.context = c
		return Reply{}, nil
	}
	if s.context == nil {
		return Reply{}, errors.New("native uninitialized")
	}
	var pathRoot unix.Stat_t
	if err := unix.Lstat(s.context.ConfigHome, &pathRoot); err != nil || pathRoot.Mode&unix.S_IFMT != unix.S_IFDIR || uint64(pathRoot.Dev) != s.context.Root.Device || pathRoot.Ino != s.context.Root.Inode || pathRoot.Uid != s.context.Root.UID || pathRoot.Mode&0o022 != 0 {
		return Reply{Code: "stale"}, nil
	}

	if request.Op != "claim-lease" && request.Op != "capture-rollback-support" && request.Op != "observe-legacy-checkpoint" && len(files) != 0 {
		return Reply{}, errors.New("unexpected native descriptors")
	}
	if request.Binding == nil {
		if request.Session != s.context.Session {
			return Reply{}, errors.New("native session binding")
		}
	}
	if request.Op == "capture-rollback-support" {
		return s.captureRollbackSupport(ctx, request, files)
	}
	if request.Op == "observe-legacy-checkpoint" {
		return s.observeLegacyCheckpoint(ctx, request, files)
	}
	if request.Op == "claim-lease" {
		b := request.Binding
		if !bindingValid(b) || b.Session != s.context.Session || b.Artifact != s.context.Artifact || b.Helper != s.image || s.binding != nil || request.Update == nil {
			return Reply{}, errors.New("native lease binding")
		}
		var owner *Identity
		if len(request.Owner) != 0 && string(request.Owner) != "null" {
			var value Identity
			if strict(request.Owner, &value) != nil {
				return Reply{}, errors.New("owner lease identity")
			}
			owner = &value
		}
		count := 1
		if owner != nil {
			count = 2
		}
		if len(files) != count {
			return Reply{}, errors.New("native descriptor roles")
		}
		update, e := IdentityOf(files[0])
		if e != nil || update != *request.Update || !regular(update) {
			return Reply{}, errors.New("update lease identity")
		}
		// Check the received OFD refers to the exact pinned-root lock leaf. Never
		// LOCK_UN it, and never reopen procfd as a substitute for the borrowed OFD.
		directory, e := unix.Openat(int(s.root.Fd()), "migrations", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if e != nil {
			return Reply{}, e
		}
		if e = privateDirectory(directory); e != nil {
			unix.Close(directory)
			return Reply{}, e
		}
		var stat unix.Stat_t
		e = unix.Fstatat(directory, "update.lock", &stat, unix.AT_SYMLINK_NOFOLLOW)
		unix.Close(directory)
		if e != nil || uint64(stat.Dev) != update.Device || stat.Ino != update.Inode {
			return Reply{}, errors.New("foreign update lease")
		}
		if owner != nil {
			actual, e := IdentityOf(files[1])
			if e != nil || actual != *owner || !regular(actual) {
				return Reply{}, errors.New("owner lease identity")
			}
		}
		for _, file := range files {
			dup, e := retain(file)
			if e != nil {
				closeFiles(s.leases)
				s.leases = nil
				return Reply{}, e
			}
			s.leases = append(s.leases, dup)
		}
		if owner != nil {
			release, err := uninstall.RegisterBorrowedOwnerLock(s.context.DataHome, s.leases[1])
			if err != nil {
				s.releaseLeases()
				return Reply{}, err
			}
			s.ownerRelease = release
		}
		s.binding = b
		return Reply{}, nil
	}
	if request.Binding != nil {
		if !bindingValid(request.Binding) || s.binding == nil || *request.Binding != *s.binding {
			return Reply{}, errors.New("native mutation binding")
		}
	}
	unavailable := func(err error) (Reply, error) {
		if errors.Is(err, domain.ErrPlanStale) {
			return Reply{Code: "stale", Diagnostic: componentDiagnostic(err)}, nil
		}
		if err != nil {
			return Reply{Code: "unavailable", Diagnostic: componentDiagnostic(err)}, nil
		}
		return Reply{}, nil
	}
	switch request.Op {
	case "read-installed-recipe", "bind-installed-recipe":
		return s.installedRecipe(ctx, request)
	case "capture-config-layout":
		value, err := config.ReadPersistentSnapshotAt(s.root, ".sync/manifest.json")
		if err != nil {
			return unavailable(err)
		}
		if len(value.Content) > 1<<20 {
			return Reply{}, errors.New("config layout snapshot bound")
		}
		return Reply{Layout: &ConfigLayoutSnapshot{Exists: value.Exists, Raw: base64.StdEncoding.EncodeToString(value.Content), Digest: digest(value.Content)}}, nil
	case "capture-ingress":
		var value IngressFacts
		var err error
		if request.LegacyRust {
			reader, ok := s.components.(interface {
				CaptureRustIngress(context.Context, *IngressBinding) (IngressFacts, error)
			})
			if !ok {
				return Reply{}, errors.New("native Rust ingress reader unavailable")
			}
			value, err = reader.CaptureRustIngress(ctx, request.IngressBinding)
		} else {
			value, err = s.components.CaptureIngress(ctx, request.IngressBinding)
		}
		if err != nil {
			return unavailable(err)
		}
		return Reply{Ingress: &value}, nil
	case "observe-ingress":
		if request.Step == nil {
			return Reply{}, errors.New("missing ingress recipe")
		}
		value, err := s.components.ObserveIngress(ctx, *request.Step)
		if err != nil {
			return unavailable(err)
		}
		return Reply{Actual: value}, nil
	case "verify-ingress":
		if request.Step == nil {
			return Reply{}, errors.New("missing ingress recipe")
		}
		return unavailable(s.components.VerifyIngress(ctx, *request.Step))
	case "apply-ingress":
		if request.Step == nil {
			return Reply{}, errors.New("missing ingress recipe")
		}
		return s.apply(ctx, request, "source-ingress", request.Step.Kind, func() error { return s.components.ApplyIngress(s.effectContext, *request.Step) })
	case "capture-inventory":
		yards, owners, e := s.components.CaptureInventory(ctx)
		if e != nil {
			return unavailable(e)
		}
		if len(yards) > 128 || len(owners) > 256 {
			return Reply{}, errors.New("native inventory bound")
		}
		return Reply{Yards: yards, Owners: owners}, nil
	case "capture-yard":
		if !domain.SafeName(request.Yard) {
			return Reply{}, errors.New("native yard")
		}
		value, e := s.components.CaptureYard(ctx, request.Yard, request.Resolved)
		if e != nil {
			return unavailable(e)
		}
		return Reply{Settings: &value}, nil
	case "prepare-settings":
		if !domain.SafeName(request.Yard) || len(request.Edits) == 0 || len(request.Edits) > 256 {
			return Reply{}, errors.New("native settings preparation")
		}
		value, e := s.components.PrepareSettings(ctx, request.Yard, request.Edits)
		if e != nil {
			return unavailable(e)
		}
		return Reply{Recipe: &value}, nil
	case "capture-owner":
		value, e := s.components.CaptureOwner(ctx)
		if e != nil {
			return unavailable(e)
		}
		return Reply{Owner: &value}, nil
	case "project-activation":
		projector, ok := s.components.(ActivationProjector)
		if !ok || request.Before == nil || request.Before.PreparedRecipe != "" && !fingerprint(request.Before.PreparedRecipe) {
			return Reply{}, errors.New("missing original activation projection capability")
		}
		yards, e := projector.ProjectActivation(ctx, *request.Before)
		if e != nil {
			return unavailable(e)
		}
		if len(yards) > 128 || !slices.IsSorted(yards) {
			return Reply{}, errors.New("activation projection inventory bound")
		}
		for index, yard := range yards {
			if !domain.SafeName(yard) || index > 0 && yards[index-1] == yard {
				return Reply{}, errors.New("activation projection inventory")
			}
		}
		return Reply{Yards: yards}, nil
	case "observe-owner":
		reader, ok := s.components.(OwnerProgressReader)
		if !ok || request.Before == nil || len(request.Token) != 32 || strings.Trim(request.Token, "0123456789abcdef") != "" || request.Before.PreparedRecipe != "" && !fingerprint(request.Before.PreparedRecipe) {
			return Reply{}, errors.New("missing original owner observation capability")
		}
		actual, e := reader.ObserveOwner(ctx, *request.Before, request.Token)
		if e != nil {
			return unavailable(e)
		}
		if actual != "expected" && actual != "in-progress" && actual != "desired" {
			return Reply{}, errors.New("invalid owner progress")
		}
		return Reply{Actual: actual}, nil
	case "capture-activation":
		var owner string
		if strict(request.Owner, &owner) != nil || !domain.SafeID(owner) {
			return Reply{}, errors.New("native activation owner")
		}
		targets, e := s.components.CaptureActivation(ctx, owner, request.Fresh)
		if e != nil {
			return unavailable(e)
		}
		if len(targets) > 128 {
			return Reply{}, errors.New("native target bound")
		}
		return Reply{Targets: targets}, nil
	case "project-settings":
		if request.Recipe == nil || len(request.Edits) == 0 || len(request.Edits) > 2 {
			return Reply{}, errors.New("missing projection recipe")
		}
		actual, e := s.components.ProjectSettings(ctx, *request.Recipe, request.Edits)
		if e != nil {
			return unavailable(e)
		}
		return Reply{Actual: actual}, nil
	case "capture-settings":
		if request.Recipe == nil {
			return Reply{}, errors.New("missing capture recipe")
		}
		actual, e := s.components.CaptureSettings(ctx, *request.Recipe)
		if e != nil {
			return unavailable(e)
		}
		return Reply{Actual: actual}, nil
	case "observe-settings":
		if request.Recipe == nil {
			return Reply{}, errors.New("missing settings recipe")
		}
		actual, e := s.components.ObserveSettings(ctx, *request.Recipe)
		if e != nil {
			return unavailable(e)
		}
		return Reply{Actual: actual}, nil
	case "apply-settings":
		if request.Recipe == nil {
			return Reply{}, errors.New("missing settings recipe")
		}
		return s.apply(ctx, request, "settings", request.Recipe.Yard, func() error { return s.components.ApplySettings(s.effectContext, *request.Recipe) })
	case "apply-owner":
		if request.Before == nil {
			return Reply{}, errors.New("missing owner recipe")
		}
		return s.apply(ctx, request, "owner-registration", "test-yard", func() error { return s.components.ApplyOwner(s.effectContext, *request.Before, request.Token) })
	case "cleanup-owner":
		cleaner, ok := s.components.(OwnerArchiveCleaner)
		if !ok || request.Before == nil || len(request.Token) != 32 || strings.Trim(request.Token, "0123456789abcdef") != "" || len(s.leases) != 2 || s.ownerRelease == nil {
			return Reply{}, errors.New("missing terminal owner cleanup capability")
		}
		return s.apply(ctx, request, "owner-registration-cleanup", "test-yard", func() error { return cleaner.CleanupOwner(s.effectContext, *request.Before, request.Token) })
	case "query-owner-cleanup":
		return s.queryOwnerCleanup(ctx, request)
	case "apply-activation":
		if request.Target == nil {
			return Reply{}, errors.New("missing activation recipe")
		}
		target := *request.Target
		return s.apply(ctx, request, target.Owner, target.Yard, func() error { return s.components.ApplyActivation(s.effectContext, target) })
	case "fence-activation":
		if request.Target == nil || (request.Target.Class != "profile-runtime" && request.Target.Class != "stage") {
			return Reply{}, errors.New("missing native fence target")
		}
		if _, err := fenceObserved(*request.Target, request.Successor); err != nil {
			return Reply{}, err
		}
		component, ok := s.components.(interface {
			FenceActivation(context.Context, Target, string) error
		})
		if !ok {
			return Reply{Code: "unavailable"}, nil
		}
		target := *request.Target
		return s.apply(ctx, request, target.Owner, target.Yard, func() error { return component.FenceActivation(s.effectContext, target, request.Successor) })
	case "resolve-after-owner":
		return s.resolveAfterOwner(ctx, request)
	case "resolve-owner-target":
		if request.Target == nil || s.ownerResultsPayload == "" {
			return Reply{Code: "unavailable"}, nil
		}
		if request.Binding != nil {
			if err := s.checkOwnerResultEffectBinding(*request.Binding); err != nil {
				return Reply{}, err
			}
		}
		resolver, ok := s.components.(interface {
			ResolveOwnerTarget(context.Context, Target) (Target, error)
		})
		if !ok {
			return Reply{Code: "unavailable"}, nil
		}
		target, err := resolver.ResolveOwnerTarget(ctx, *request.Target)
		if err != nil {
			return unavailable(err)
		}
		comparison := target
		comparison.InstanceUUID = request.Target.InstanceUUID
		if comparison != *request.Target || target.InstanceUUID == "" || request.Target.InstanceUUID != "absent" && target.InstanceUUID != request.Target.InstanceUUID {
			return Reply{}, errors.New("owner target projection differs")
		}
		return Reply{Targets: []Target{target}}, nil
	case "bind-owner-results":
		return s.bindOwnerResults(ctx, request)
	case "query-invocation":
		return s.queryInvocation(ctx, *request.Binding)
	case "release-lease":
		s.releaseLeases()
		return Reply{}, nil
	}
	return Reply{}, errors.New("native operation")
}

// apply is entered only with a physically borrowed, session-bound lease and an
// exact durable owner intent. A missing terminal witness is never inferred away.
func (s *session) apply(ctx context.Context, request Request, owner, resource string, apply func() error) (Reply, error) {
	if request.Binding == nil || s.binding == nil {
		return Reply{}, errors.New("native effect without custody")
	}
	if ctx.Err() != nil {
		return Reply{Code: "unavailable"}, nil
	}
	if err := s.checkOwnerResultEffectBinding(*request.Binding); err != nil {
		return Reply{}, err
	}
	if request.Target != nil && request.Target.AfterOwner != "" {
		if _, err := s.ownerCompletion(); err != nil {
			return Reply{Code: "unknown"}, nil
		}
	}
	if _, e := s.readTerminal(*request.Binding); e == nil {
		return Reply{Code: "stale"}, nil
	}
	directory, err := s.factsDirectory(*request.Binding)
	if err != nil {
		return Reply{}, err
	}
	_, priorErr := readFactAt(directory, factName(request.Binding.Ordinal, "accepted"))
	unix.Close(directory)
	if priorErr == nil {
		return Reply{Code: "unknown"}, nil
	}
	if !errors.Is(priorErr, unix.ENOENT) {
		return Reply{}, priorErr
	}
	recipe, err := recipeFingerprint(request)
	if err != nil {
		return Reply{}, err
	}
	if err := s.checkIntent(*request.Binding, owner, resource, recipe); err != nil {
		return Reply{}, err
	}
	accepted := Terminal{Version: 1, Binding: *request.Binding, Owner: owner, Resource: resource, State: "accepted", Operations: []ports.NativeOperation{}}
	if err := s.writeFact(*request.Binding, "accepted", accepted); err != nil {
		return Reply{}, err
	}
	observer := &invocationObserver{session: s, binding: *request.Binding, ownerToken: request.Token}
	// Owner EOF closes admission but must not kill a local CLI transport while
	// its remote script is still running. Accepted work keeps its own bounded
	// drain budget. Release refresh already permits up to 75 minutes (the
	// ordinary prepared command boundary); retain that source-owned allowance
	// plus final-state/publication reserve. The owner exchange permits 76 minutes.
	deadline := time.Now().Add(75*time.Minute + 30*time.Second)
	if parentDeadline, ok := ctx.Deadline(); ok && parentDeadline.Before(deadline) {
		deadline = parentDeadline
	}
	ctx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	defer cancel()
	ctx = ports.WithNativeOperationObserver(ctx, observer)
	bound, _ := json.Marshal(request.Binding)
	ctx = ports.WithNativeInvocationKey(ctx, digest(bound))
	ctx = ports.WithNativeOwnerHome(ctx, s.context.DataHome)
	// The closure receives this context through the per-invocation argument.
	s.effectContext = ctx
	defer func() { s.effectContext = nil }()
	if err := apply(); err != nil {
		diagnostic := componentDiagnostic(err)
		if observer.failed(err) || request.Op == "cleanup-owner" && observer.localOnly() {
			failed := Terminal{Version: 1, Binding: *request.Binding, Owner: owner, Resource: resource, State: "failed", Operations: observer.operations()}
			if err := s.writeTerminal(failed); err == nil {
				return Reply{Code: "failed", Diagnostic: diagnostic, Terminal: &failed}, nil
			}
		}
		return Reply{Code: "unknown", Diagnostic: diagnostic}, nil
	}
	if request.Op == "cleanup-owner" && !observer.localOnly() {
		return Reply{Code: "unknown"}, nil
	}
	if err := observer.closed(); err != nil {
		return Reply{Code: "unknown"}, nil
	}
	if request.Recipe != nil {
		actual, err := s.components.ObserveSettings(ctx, *request.Recipe)
		if err != nil || actual != "desired" {
			return Reply{Code: "unknown", Diagnostic: componentDiagnostic(err)}, nil
		}
	}
	if request.Before != nil && request.Op != "cleanup-owner" {
		reader, ok := s.components.(OwnerProgressReader)
		if !ok {
			return Reply{Code: "unknown"}, nil
		}
		actual, err := reader.ObserveOwner(ctx, *request.Before, request.Token)
		if err != nil || actual != "desired" {
			return Reply{Code: "unknown", Diagnostic: componentDiagnostic(err)}, nil
		}
	}
	if request.Target != nil && request.Op != "fence-activation" {
		targets, err := s.components.CaptureActivation(ctx, request.Target.Owner, true)
		verified := false
		for _, value := range targets {
			if value.Yard == request.Target.Yard && value.Class == request.Target.Class && value.Project == request.Target.Project && value.Instance == request.Target.Instance && value.InstanceUUID == request.Target.InstanceUUID && value.Desired == request.Target.Desired && value.Source == request.Target.Source && value.State != "stale" {
				verified = true
			}
		}
		if err != nil || !verified {
			return Reply{Code: "unknown", Diagnostic: componentDiagnostic(err)}, nil
		}
	}
	if err := observer.closed(); err != nil {
		return Reply{Code: "unknown"}, nil
	}
	terminal := Terminal{Version: 1, Binding: *request.Binding, Owner: owner, Resource: resource, State: "complete", Operations: observer.operations()}
	if request.Target != nil {
		terminal.Project = request.Target.Project
		terminal.Instance = request.Target.Instance
		terminal.InstanceUUID = request.Target.InstanceUUID
		terminal.Observed = request.Target.Desired
		if request.Op == "fence-activation" {
			terminal.Observed, err = fenceObserved(*request.Target, request.Successor)
			if err != nil {
				return Reply{Code: "unknown"}, nil
			}
		}
	}
	if err := s.writeTerminal(terminal); err != nil {
		return Reply{Code: "unknown"}, nil
	}
	return Reply{Terminal: &terminal}, nil
}
func digest(raw []byte) string { s := sha256.Sum256(raw); return hex.EncodeToString(s[:]) }

func recipeFingerprint(request Request) (string, error) {
	var value any
	switch request.Op {
	case "apply-settings":
		value = request.Recipe
	case "apply-activation":
		value = request.Target
	case "fence-activation":
		value = struct {
			Target    *Target `json:"target"`
			Successor string  `json:"successor"`
		}{request.Target, request.Successor}
	case "cleanup-owner", "query-owner-cleanup":
		value = struct {
			Kind   string       `json:"kind"`
			Before *OwnerBefore `json:"before"`
			Token  string       `json:"token"`
		}{"cleanup-owner", request.Before, request.Token}
	case "apply-owner":
		value = struct {
			Before *OwnerBefore `json:"before"`
			Token  string       `json:"token"`
		}{request.Before, request.Token}
	case "apply-ingress":
		value = request.Step
	default:
		return "", errors.New("native recipe kind")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return digest(raw), nil
}
