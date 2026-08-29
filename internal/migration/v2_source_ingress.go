package migration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

const maximumSourceIngressBytes = 32 << 20

type V2SourceIngressOptions struct {
	Descriptor     releasetransition.SourceIngressRequest
	RepositoryRoot string
	RuntimeRoot    string
	ConfigHome     string
	Environment    []string
	Stderr         io.Writer
}

type sourceIngressRunner func(context.Context, releasetransition.V2IngressStep) error

type V2SourceIngress struct {
	options      V2SourceIngressOptions
	trustedHome  string
	recoveryRoot string
	run          sourceIngressRunner
}

type sourceIngressState struct {
	base     releasetransition.Fingerprint
	manifest SourceInstallManifest
	imported bool
	complete bool
}

func NewV2SourceIngress(options V2SourceIngressOptions) (*V2SourceIngress, error) {
	account, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("resolve source ingress account: %w", err)
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil || uint32(uid) != uint32(os.Geteuid()) {
		return nil, errors.New("source ingress account does not match the effective user")
	}
	return newV2SourceIngress(options, account.HomeDir, nil)
}

func newV2SourceIngress(
	options V2SourceIngressOptions,
	trustedHome string,
	runner sourceIngressRunner,
) (*V2SourceIngress, error) {
	if err := options.Descriptor.Validate(); err != nil {
		return nil, err
	}
	if !validV1ImportRoot(options.RepositoryRoot) || !validV1ImportRoot(options.RuntimeRoot) ||
		!validV1ImportRoot(options.ConfigHome) || !validV1ImportRoot(trustedHome) {
		return nil, errors.New("source ingress requires absolute non-root trusted roots")
	}
	trustedHome = filepath.Clean(trustedHome)
	roles := []string{
		options.Descriptor.SourceRoot, options.Descriptor.DataHome,
		options.Descriptor.BinDir, options.Descriptor.RC, options.Descriptor.LoginRC,
		options.RuntimeRoot, options.ConfigHome,
	}
	for _, path := range roles {
		if filepath.Clean(path) == trustedHome || !pathWithin(path, trustedHome) {
			return nil, errors.New("source ingress path escapes the operating-system account home")
		}
	}
	if !sourceIngressCandidateWithinReleaseStore(options.RepositoryRoot, options.RuntimeRoot) {
		return nil, errors.New("source ingress candidate escapes the managed release store")
	}
	if _, err := realOwnedDirectory(trustedHome, true); err != nil {
		return nil, fmt.Errorf("source ingress account home: %w", err)
	}
	ingress := &V2SourceIngress{
		options: options, trustedHome: trustedHome,
		recoveryRoot: filepath.Join(options.Descriptor.DataHome, "recovery", "pre-go-source"),
	}
	if runner == nil {
		ingress.run = ingress.runScript
	} else {
		ingress.run = runner
	}
	return ingress, nil
}

func sourceIngressCandidateWithinReleaseStore(repositoryRoot, runtimeRoot string) bool {
	releaseStore := filepath.Join(runtimeRoot, "releases")
	resolvedStore, err := filepath.EvalSymlinks(releaseStore)
	if err != nil {
		return false
	}
	if pathWithin(repositoryRoot, releaseStore) {
		resolved, err := filepath.EvalSymlinks(repositoryRoot)
		return err == nil && pathWithin(resolved, resolvedStore)
	}
	clean := filepath.Clean(repositoryRoot)
	for _, prefix := range []string{
		"/proc/self/fd/", fmt.Sprintf("/proc/%d/fd/", os.Getppid()),
	} {
		descriptor, found := strings.CutPrefix(clean, prefix)
		if !found {
			continue
		}
		if _, err := strconv.ParseUint(descriptor, 10, 64); err != nil {
			return false
		}
		resolved, err := filepath.EvalSymlinks(clean)
		return err == nil && pathWithin(resolved, resolvedStore)
	}
	return false
}

func (ingress *V2SourceIngress) Inspect(
	_ context.Context,
	binding *releasetransition.V2IngressBinding,
) (releasetransition.V2IngressInspection, error) {
	state, err := ingress.inspectState()
	if err != nil {
		return releasetransition.V2IngressInspection{}, err
	}
	importExpected, importDesired, entryExpected, entryDesired := sourceIngressFingerprints(state.base)
	bound := make(map[releasetransition.V2IngressOperationKind]bool, 2)
	if binding != nil {
		for _, step := range binding.Steps {
			var expected, desired releasetransition.Fingerprint
			switch step.Kind {
			case releasetransition.V2SourceImport:
				expected, desired = importExpected, importDesired
			case releasetransition.V2SourceEntrypoints:
				expected, desired = entryExpected, entryDesired
			default:
				continue
			}
			if bound[step.Kind] {
				return releasetransition.V2IngressInspection{}, errors.New("source ingress plan binding is duplicated")
			}
			if step.Expected != expected || step.Desired != desired {
				return releasetransition.V2IngressInspection{}, errors.New("source ingress plan binding changed")
			}
			bound[step.Kind] = true
		}
	}
	operations := make([]releasetransition.V2IngressOperation, 0, 2)
	if bound[releasetransition.V2SourceImport] || !state.imported {
		operations = append(operations, releasetransition.V2IngressOperation{
			Kind: releasetransition.V2SourceImport, Decision: releasetransition.DecisionCanonicalize,
			Expected: importExpected, Desired: importDesired,
		})
	}
	if bound[releasetransition.V2SourceEntrypoints] || !state.complete {
		operations = append(operations, releasetransition.V2IngressOperation{
			Kind: releasetransition.V2SourceEntrypoints, Decision: releasetransition.DecisionCanonicalize,
			Expected: entryExpected, Desired: entryDesired,
		})
	}
	if len(operations) == 0 {
		return releasetransition.V2IngressInspection{}, nil
	}
	return releasetransition.V2IngressInspection{
		Operations: operations,
		Decisions: []releasetransition.RedactedDecision{
			{Resource: "source-install.config", Scope: "source-install", Decision: releasetransition.DecisionCanonicalize, Result: "persistent-config"},
			{Resource: "source-install.entrypoints", Scope: "source-install", Decision: releasetransition.DecisionCanonicalize, Result: "stable-runtime"},
		},
		Prospective: &sourceSettingsView{ingress: ingress, state: state},
	}, nil
}

func (ingress *V2SourceIngress) Observe(
	_ context.Context,
	step releasetransition.V2IngressStep,
) (releasetransition.Fingerprint, error) {
	state, err := ingress.inspectState()
	if err != nil {
		return "", err
	}
	importExpected, importDesired, entryExpected, entryDesired := sourceIngressFingerprints(state.base)
	switch step.Kind {
	case releasetransition.V2SourceImport:
		if step.Expected != importExpected || step.Desired != importDesired {
			return "", errors.New("source import binding changed")
		}
		if state.imported {
			return importDesired, nil
		}
		return importExpected, nil
	case releasetransition.V2SourceEntrypoints:
		if step.Expected != entryExpected || step.Desired != entryDesired {
			return "", errors.New("source entrypoint binding changed")
		}
		if state.complete {
			return entryDesired, nil
		}
		return entryExpected, nil
	default:
		return "", errors.New("source ingress received an unsupported operation")
	}
}

func (ingress *V2SourceIngress) Apply(
	ctx context.Context,
	step releasetransition.V2IngressStep,
) error {
	actual, err := ingress.Observe(ctx, step)
	if err != nil || actual == step.Desired {
		return err
	}
	if actual != step.Expected {
		return errors.New("source ingress is in a third state")
	}
	return ingress.run(ctx, step)
}

func (ingress *V2SourceIngress) Verify(
	ctx context.Context,
	step releasetransition.V2IngressStep,
) error {
	actual, err := ingress.Observe(ctx, step)
	if err != nil {
		return err
	}
	if actual != step.Desired {
		return errors.New("source ingress operation did not reach its desired state")
	}
	return nil
}

func (ingress *V2SourceIngress) inspectState() (sourceIngressState, error) {
	info, err := os.Lstat(ingress.recoveryRoot)
	if errors.Is(err, os.ErrNotExist) {
		manifest, discoverErr := DiscoverSourceInstallWithRoots(
			ingress.options.Descriptor.SourceRoot,
			ingress.options.Descriptor.DataHome,
			ingress.options.ConfigHome,
		)
		if discoverErr != nil {
			return sourceIngressState{}, discoverErr
		}
		if err := ingress.verifySourceLinks(); err != nil {
			return sourceIngressState{}, err
		}
		base, err := sourceIngressBase(
			ingress.options.Descriptor, ingress.options.ConfigHome, "", manifest,
		)
		return sourceIngressState{base: base, manifest: manifest}, err
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return sourceIngressState{}, errors.New("source ingress recovery root is unsafe")
	}
	manifestPayload, err := readV1ImportProtectedFile(
		filepath.Join(ingress.recoveryRoot, "source-install-manifest.json"),
		maximumSourceIngressBytes,
	)
	if err != nil {
		return sourceIngressState{}, err
	}
	var manifest SourceInstallManifest
	decoder := json.NewDecoder(bytes.NewReader(manifestPayload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return sourceIngressState{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return sourceIngressState{}, errors.New("source ingress recovery manifest has trailing data")
	}
	if manifest.SourceRoot != ingress.options.Descriptor.SourceRoot ||
		manifest.DataHome != ingress.options.Descriptor.DataHome ||
		manifest.ConfigHome != ingress.options.ConfigHome {
		return sourceIngressState{}, errors.New("source ingress recovery descriptor changed")
	}
	observedBase, err := sourceIngressBase(
		ingress.options.Descriptor, ingress.options.ConfigHome, ingress.recoveryRoot, manifest,
	)
	if err != nil {
		return sourceIngressState{}, errors.New("source ingress recovery does not match its inspected plan")
	}
	basePayload, err := readV1ImportProtectedFile(
		filepath.Join(ingress.recoveryRoot, "source-plan"), 256,
	)
	predecessor := errors.Is(err, os.ErrNotExist)
	base := observedBase
	if predecessor {
		for _, name := range []string{"outer-transaction", "outer-plan", "source-import.state"} {
			if _, statErr := os.Lstat(filepath.Join(ingress.recoveryRoot, name)); !errors.Is(statErr, os.ErrNotExist) {
				return sourceIngressState{}, errors.New("source ingress recovery binding is incomplete")
			}
		}
	} else {
		if err != nil {
			return sourceIngressState{}, err
		}
		base = releasetransition.Fingerprint(strings.TrimSpace(string(basePayload)))
		if len(base) != sha256.Size*2 {
			return sourceIngressState{}, errors.New("source ingress recovery plan is invalid")
		}
		if _, err := hex.DecodeString(string(base)); err != nil {
			return sourceIngressState{}, errors.New("source ingress recovery plan is invalid")
		}
		if observedBase != base {
			return sourceIngressState{}, errors.New("source ingress recovery does not match its inspected plan")
		}
	}
	phase, err := ingress.readRecoveryPhase()
	if err != nil {
		return sourceIngressState{}, err
	}
	state := sourceIngressState{base: base, manifest: manifest}
	if predecessor {
		switch phase {
		case "none", "config-import", "legacy-archive", "state-migration",
			"shell-integration", "entrypoint-switch":
		default:
			return sourceIngressState{}, errors.New("predecessor source ingress recovery phase is unsupported")
		}
	} else {
		switch phase {
		case "source-import-ready", "shell-integration", "entrypoint-switch":
			state.imported = true
		case "complete":
			state.imported, state.complete = true, true
		case "none", "config-import", "legacy-archive":
		default:
			return sourceIngressState{}, errors.New("source ingress recovery phase is unsupported")
		}
	}
	if err := ingress.verifyRecoveryResources(phase, manifest, predecessor); err != nil {
		return sourceIngressState{}, err
	}
	return state, nil
}

func (ingress *V2SourceIngress) verifyRecoveryResources(
	phase string,
	manifest SourceInstallManifest,
	predecessor bool,
) error {
	switch phase {
	case "none", "config-import", "legacy-archive":
		return ingress.verifySourceLinks()
	}
	if err := ingress.verifyImportedResources(manifest, !predecessor); err != nil {
		return err
	}
	rc, err := ingress.observeRecoveryFile("rc", ingress.options.Descriptor.RC)
	if err != nil {
		return err
	}
	login := rc
	if ingress.options.Descriptor.LoginRC != ingress.options.Descriptor.RC {
		login, err = ingress.observeRecoveryFile("login-rc", ingress.options.Descriptor.LoginRC)
		if err != nil {
			return err
		}
	}
	yard, err := ingress.observeRecoveryLink("yard")
	if err != nil {
		return err
	}
	sy, err := ingress.observeRecoveryLink("sy")
	if err != nil {
		return err
	}
	switch phase {
	case "source-import-ready", "state-migration":
		if !rc.before || !login.before || !yard.before || !sy.before {
			return errors.New("source ingress import checkpoint does not match its resources")
		}
	case "shell-integration":
		if !yard.before || !sy.before || login.after && !rc.after {
			return errors.New("source ingress shell checkpoint does not match its resources")
		}
	case "entrypoint-switch":
		if !rc.after || !login.after || yard.after && !sy.after {
			return errors.New("source ingress entrypoint checkpoint does not match its resources")
		}
	case "complete":
		if !rc.after || !login.after || !yard.after || !sy.after {
			return errors.New("source ingress completion does not match its resources")
		}
	}
	return nil
}

func (ingress *V2SourceIngress) verifyImportedResources(
	manifest SourceInstallManifest,
	requireSeal bool,
) error {
	if requireSeal {
		marker, err := readV1ImportProtectedFile(
			filepath.Join(ingress.recoveryRoot, "source-import.state"), 64,
		)
		if err != nil || strings.TrimSpace(string(marker)) != "committed" {
			return errors.New("source ingress import checkpoint is not sealed")
		}
	}
	for _, entry := range manifest.Entries {
		if entry.DestinationRoot != DestinationConfigHome ||
			!safeRelativePath(filepath.FromSlash(entry.Destination)) {
			return errors.New("source ingress manifest has an unsafe destination")
		}
		expected, err := sourceIngressEntryPayload(
			ingress.options.Descriptor, ingress.options.ConfigHome, ingress.recoveryRoot, entry,
		)
		if err != nil {
			return err
		}
		path := filepath.Join(ingress.options.ConfigHome, filepath.FromSlash(entry.Destination))
		actual, mode, err := readSourceIngressFile(path)
		if err != nil || mode != 0o600 || !bytes.Equal(actual, expected) {
			return fmt.Errorf("source ingress imported result changed: %s", entry.Destination)
		}
	}
	if err := ingress.verifyLegacyArchive(
		"legacy-data-config", filepath.Join(ingress.options.Descriptor.DataHome, "config.env"), false,
	); err != nil {
		return err
	}
	return ingress.verifyLegacyArchive(
		"legacy-operator-overlay",
		filepath.Join(ingress.options.Descriptor.DataHome, "operator-overlay"), true,
	)
}

func (ingress *V2SourceIngress) verifyLegacyArchive(
	label string,
	expectedPath string,
	directory bool,
) error {
	path, err := readV1ImportProtectedFile(filepath.Join(ingress.recoveryRoot, label+".path"), 4096)
	if err != nil || strings.TrimSpace(string(path)) != expectedPath {
		return fmt.Errorf("source ingress %s archive path changed", label)
	}
	state, err := readV1ImportProtectedFile(filepath.Join(ingress.recoveryRoot, label+".state"), 64)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(expectedPath); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("source ingress %s remained active", label)
	}
	backup := filepath.Join(ingress.recoveryRoot, label+".before")
	switch strings.TrimSpace(string(state)) {
	case "absent":
		if _, err := os.Lstat(backup); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("source ingress %s has an unexpected archive", label)
		}
	case "present":
		if directory {
			if _, err := realOwnedDirectory(backup, true); err != nil {
				return fmt.Errorf("source ingress %s archive is unsafe", label)
			}
		} else if err := validateSourceFile(backup); err != nil {
			return fmt.Errorf("source ingress %s archive is unsafe", label)
		}
	default:
		return fmt.Errorf("source ingress %s archive state is invalid", label)
	}
	return nil
}

type sourceIngressProgress struct {
	before bool
	after  bool
}

func (ingress *V2SourceIngress) observeRecoveryFile(
	label string,
	expectedPath string,
) (sourceIngressProgress, error) {
	path, err := readV1ImportProtectedFile(filepath.Join(ingress.recoveryRoot, label+".path"), 4096)
	if err != nil || strings.TrimSpace(string(path)) != expectedPath {
		return sourceIngressProgress{}, fmt.Errorf("source ingress %s path changed", label)
	}
	state, err := readV1ImportProtectedFile(filepath.Join(ingress.recoveryRoot, label+".state"), 64)
	if err != nil {
		return sourceIngressProgress{}, err
	}
	progress := sourceIngressProgress{}
	switch strings.TrimSpace(string(state)) {
	case "absent":
		_, err := os.Lstat(expectedPath)
		progress.before = errors.Is(err, os.ErrNotExist)
	case "present":
		progress.before, err = sourceIngressFilesEqual(
			expectedPath, filepath.Join(ingress.recoveryRoot, label+".before"),
		)
		if err != nil {
			return sourceIngressProgress{}, err
		}
	default:
		return sourceIngressProgress{}, fmt.Errorf("source ingress %s state is invalid", label)
	}
	progress.after, err = sourceIngressFilesEqual(
		expectedPath, filepath.Join(ingress.recoveryRoot, label+".after"),
	)
	if err != nil {
		return sourceIngressProgress{}, err
	}
	if !progress.before && !progress.after {
		return sourceIngressProgress{}, fmt.Errorf("source ingress %s file is in a third state", label)
	}
	return progress, nil
}

func (ingress *V2SourceIngress) observeRecoveryLink(name string) (sourceIngressProgress, error) {
	original, err := readV1ImportProtectedFile(
		filepath.Join(ingress.recoveryRoot, name+".target"), 4096,
	)
	if err != nil {
		return sourceIngressProgress{}, err
	}
	desired, err := readV1ImportProtectedFile(filepath.Join(ingress.recoveryRoot, "runtime-launcher"), 4096)
	if err != nil {
		return sourceIngressProgress{}, err
	}
	path := filepath.Join(ingress.options.Descriptor.BinDir, name)
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return sourceIngressProgress{}, fmt.Errorf("source ingress %s launcher is unsafe", name)
	}
	target, err := os.Readlink(path)
	if err != nil {
		return sourceIngressProgress{}, err
	}
	progress := sourceIngressProgress{
		before: target == strings.TrimSpace(string(original)),
		after:  target == strings.TrimSpace(string(desired)),
	}
	if !progress.before && !progress.after {
		return sourceIngressProgress{}, fmt.Errorf("source ingress %s launcher is in a third state", name)
	}
	return progress, nil
}

func sourceIngressFilesEqual(left, right string) (bool, error) {
	leftPayload, leftMode, leftErr := readSourceIngressFile(left)
	if errors.Is(leftErr, os.ErrNotExist) {
		return false, nil
	}
	if leftErr != nil {
		return false, leftErr
	}
	rightPayload, rightMode, err := readSourceIngressFile(right)
	if err != nil {
		return false, err
	}
	return leftMode == rightMode && bytes.Equal(leftPayload, rightPayload), nil
}

func readSourceIngressFile(path string) ([]byte, os.FileMode, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Size() > maximumSourceIngressBytes {
		return nil, 0, errors.New("source ingress result is not a bounded regular file")
	}
	if err := validateOwnedSafeMode(path, info); err != nil {
		return nil, 0, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() ||
		opened.Size() > maximumSourceIngressBytes {
		return nil, 0, errors.New("source ingress result changed while opening")
	}
	payload, err := io.ReadAll(io.LimitReader(file, maximumSourceIngressBytes+1))
	if err != nil {
		return nil, 0, err
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(opened, after) || int64(len(payload)) != after.Size() ||
		after.Mode() != opened.Mode() {
		return nil, 0, errors.New("source ingress result changed while reading")
	}
	return payload, opened.Mode().Perm(), nil
}

func (ingress *V2SourceIngress) readRecoveryPhase() (string, error) {
	payload, err := readV1ImportProtectedFile(filepath.Join(ingress.recoveryRoot, "transaction"), 1024)
	if err != nil {
		return "", err
	}
	schema, phase, step := "", "", ""
	for _, line := range strings.Split(strings.TrimSpace(string(payload)), "\n") {
		key, value, present := strings.Cut(line, "=")
		if !present || value == "" {
			return "", errors.New("source ingress recovery transaction is invalid")
		}
		switch key {
		case "schema":
			if schema != "" {
				return "", errors.New("source ingress recovery transaction field is duplicated")
			}
			schema = value
		case "phase":
			if phase != "" {
				return "", errors.New("source ingress recovery transaction field is duplicated")
			}
			phase = value
		case "step":
			if step != "" {
				return "", errors.New("source ingress recovery transaction field is duplicated")
			}
			step = value
		default:
			return "", errors.New("source ingress recovery transaction has an unknown field")
		}
	}
	if schema != "1" {
		return "", errors.New("source ingress recovery transaction schema is unsupported")
	}
	switch phase + ":" + step {
	case "prepared:none", "applying:config-import", "applying:legacy-archive",
		"applying:state-migration", "applying:source-import-ready",
		"applying:shell-integration", "applying:entrypoint-switch", "complete:complete":
		return step, nil
	default:
		return "", errors.New("source ingress recovery transaction phase is invalid")
	}
}

func (ingress *V2SourceIngress) verifySourceLinks() error {
	desired := filepath.Join(ingress.options.Descriptor.SourceRoot, "bin", "yard")
	for _, name := range []string{"yard", "sy"} {
		path := filepath.Join(ingress.options.Descriptor.BinDir, name)
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || filepath.Clean(resolved) != desired {
			return errors.New("source ingress launchers do not identify the described checkout")
		}
	}
	return nil
}

func (ingress *V2SourceIngress) runScript(
	ctx context.Context,
	step releasetransition.V2IngressStep,
) error {
	operation := ""
	switch step.Kind {
	case releasetransition.V2SourceImport:
		operation = "import"
	case releasetransition.V2SourceEntrypoints:
		operation = "entrypoints"
	default:
		return errors.New("source ingress script operation is unsupported")
	}
	descriptor := ingress.options.Descriptor
	state, err := ingress.inspectState()
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx,
		filepath.Join(ingress.options.RepositoryRoot, "scripts", "migrate-source-install.sh"),
		"--runtime-root", ingress.options.RuntimeRoot,
		"--candidate-root", ingress.options.RepositoryRoot,
		"--source-root", descriptor.SourceRoot,
		"--bin-dir", descriptor.BinDir, "--rc", descriptor.RC,
		"--login-rc", descriptor.LoginRC, "--data-home", descriptor.DataHome,
		"--operation", operation, "--transaction", string(step.Binding.Transaction),
		"--plan", string(step.Binding.Plan), "--source-plan", string(state.base),
	)
	command.Env = environmentWithTrustedHome(ingress.options.Environment, ingress.trustedHome)
	command.Stdout, command.Stderr = io.Discard, ingress.options.Stderr
	return command.Run()
}

func sourceIngressBase(
	descriptor releasetransition.SourceIngressRequest,
	configHome string,
	recoveryRoot string,
	manifest SourceInstallManifest,
) (releasetransition.Fingerprint, error) {
	type entryDigest struct {
		Base, Source, Destination, Transform, Digest string
	}
	entries := make([]entryDigest, 0, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		payload, err := sourceIngressEntryPayload(descriptor, configHome, recoveryRoot, entry)
		if err != nil {
			return "", err
		}
		digest := sha256.Sum256(payload)
		entries = append(entries, entryDigest{
			Base: entry.SourceBase, Source: entry.Source, Destination: entry.Destination,
			Transform: entry.ContentTransform, Digest: hex.EncodeToString(digest[:]),
		})
	}
	sort.Slice(entries, func(left, right int) bool {
		if entries[left].Destination != entries[right].Destination {
			return entries[left].Destination < entries[right].Destination
		}
		if entries[left].Base != entries[right].Base {
			return entries[left].Base < entries[right].Base
		}
		return entries[left].Source < entries[right].Source
	})
	payload, err := json.Marshal(struct {
		SchemaVersion int                                    `json:"schemaVersion"`
		Descriptor    releasetransition.SourceIngressRequest `json:"descriptor"`
		Entries       []entryDigest                          `json:"entries"`
	}{1, descriptor, entries})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return releasetransition.Fingerprint(hex.EncodeToString(digest[:])), nil
}

func sourceIngressFingerprints(base releasetransition.Fingerprint) (
	releasetransition.Fingerprint,
	releasetransition.Fingerprint,
	releasetransition.Fingerprint,
	releasetransition.Fingerprint,
) {
	bind := func(label string) releasetransition.Fingerprint {
		digest := sha256.Sum256([]byte(string(base) + "\x00" + label))
		return releasetransition.Fingerprint(hex.EncodeToString(digest[:]))
	}
	return bind("import-before"), bind("imported"), bind("entrypoints-before"), bind("complete")
}

func environmentWithTrustedHome(environment []string, trustedHome string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, assignment := range environment {
		name, _, ok := strings.Cut(assignment, "=")
		if ok && name == "HOME" {
			continue
		}
		result = append(result, assignment)
	}
	return append(result, "HOME="+trustedHome)
}

func sourceIngressEntryPayload(
	descriptor releasetransition.SourceIngressRequest,
	configHome string,
	recoveryRoot string,
	entry SourceInstallEntry,
) ([]byte, error) {
	root := map[string]string{
		SourceBaseCheckout:   descriptor.SourceRoot,
		SourceBaseDataHome:   descriptor.DataHome,
		SourceBaseConfigHome: configHome,
	}[entry.SourceBase]
	if root == "" {
		return nil, errors.New("source ingress manifest has an unknown source base")
	}
	payload, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(entry.Source)))
	if errors.Is(err, os.ErrNotExist) && recoveryRoot != "" &&
		entry.SourceBase == SourceBaseDataHome {
		source := filepath.FromSlash(entry.Source)
		switch {
		case source == "config.env":
			payload, err = os.ReadFile(filepath.Join(recoveryRoot, "legacy-data-config.before"))
		case strings.HasPrefix(source, "operator-overlay"+string(filepath.Separator)):
			payload, err = os.ReadFile(filepath.Join(
				recoveryRoot, "legacy-operator-overlay.before",
				strings.TrimPrefix(source, "operator-overlay"+string(filepath.Separator)),
			))
		}
	}
	if err != nil {
		return nil, err
	}
	if entry.ContentTransform == ContentTransformRetiredE2EVMTemplate {
		payload = normalizeLegacyYardPayload(payload)
	}
	return payload, nil
}

func normalizeLegacyYardPayload(payload []byte) []byte {
	lines := strings.Split(string(payload), "\n")
	for index, line := range lines {
		if retiredE2EVMTemplateAssignment.MatchString(line) {
			indent := retiredE2EVMTemplateAssignment.FindStringSubmatch(line)[1]
			lines[index] = indent + "YARD_TEMPLATE=test-vms"
		}
	}
	return []byte(strings.Join(lines, "\n"))
}

type sourceSettingsView struct {
	ingress *V2SourceIngress
	state   sourceIngressState
}

func (view *sourceSettingsView) ListYards() ([]string, error) {
	names := make(map[string]struct{})
	root := filepath.Join(view.ingress.options.ConfigHome, "yards")
	entries, err := os.ReadDir(root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), ".env")
		if domain.SafeName(name) {
			names[name] = struct{}{}
		}
	}
	for _, entry := range view.state.manifest.Entries {
		parts := strings.Split(filepath.ToSlash(entry.Destination), "/")
		if len(parts) == 3 && parts[0] == "yards" && parts[2] == "config.env" && domain.SafeName(parts[1]) {
			names[parts[1]] = struct{}{}
		}
	}
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result, nil
}

func (view *sourceSettingsView) ReadSnapshot(path string) (config.PersistentFileSnapshot, error) {
	if view.state.imported {
		return config.ReadPersistentFileSnapshot(view.ingress.options.ConfigHome, path)
	}
	relative, err := filepath.Rel(view.ingress.options.ConfigHome, path)
	if err != nil || !safeRelativePath(relative) {
		return config.PersistentFileSnapshot{}, errors.New("prospective source settings path is unsafe")
	}
	for _, entry := range view.state.manifest.Entries {
		if filepath.Clean(filepath.FromSlash(entry.Destination)) != relative {
			continue
		}
		payload, err := sourceIngressEntryPayload(
			view.ingress.options.Descriptor, view.ingress.options.ConfigHome,
			view.ingress.recoveryRoot, entry,
		)
		if err != nil {
			return config.PersistentFileSnapshot{}, err
		}
		return config.PersistentFileSnapshot{Exists: true, Content: payload}, nil
	}
	return config.ReadPersistentFileSnapshot(view.ingress.options.ConfigHome, path)
}

// V2CompatibilityIngress combines the two closed historical ingress sources
// without exposing a generic plug-in registry.
type V2CompatibilityIngress struct {
	Legacy *V2LegacyIngress
	Source *V2SourceIngress
}

func (ingress *V2CompatibilityIngress) Inspect(
	ctx context.Context,
	binding *releasetransition.V2IngressBinding,
) (releasetransition.V2IngressInspection, error) {
	var result releasetransition.V2IngressInspection
	if ingress.Legacy != nil && bindingIncludesIngressKind(
		binding, releasetransition.V2LegacyV1Import,
	) {
		legacy, err := ingress.Legacy.Inspect(ctx, binding)
		if err != nil {
			return result, err
		}
		result.Operations = append(result.Operations, legacy.Operations...)
		result.Decisions = append(result.Decisions, legacy.Decisions...)
		result.Blockers = append(result.Blockers, legacy.Blockers...)
	}
	if ingress.Source != nil {
		source, err := ingress.Source.Inspect(ctx, binding)
		if err != nil {
			return result, err
		}
		result.Operations = append(result.Operations, source.Operations...)
		result.Decisions = append(result.Decisions, source.Decisions...)
		result.Blockers = append(result.Blockers, source.Blockers...)
		result.Prospective = source.Prospective
	}
	return result, nil
}

func bindingIncludesIngressKind(
	binding *releasetransition.V2IngressBinding,
	kinds ...releasetransition.V2IngressOperationKind,
) bool {
	if binding == nil {
		return true
	}
	for _, step := range binding.Steps {
		if slices.Contains(kinds, step.Kind) {
			return true
		}
	}
	return false
}

func (ingress *V2CompatibilityIngress) leaf(kind releasetransition.V2IngressOperationKind) releasetransition.V2Ingress {
	if kind == releasetransition.V2LegacyV1Import {
		return ingress.Legacy
	}
	return ingress.Source
}

func (ingress *V2CompatibilityIngress) requireLeaf(
	kind releasetransition.V2IngressOperationKind,
) (releasetransition.V2Ingress, error) {
	leaf := ingress.leaf(kind)
	if leaf == nil {
		return nil, errors.New("compatibility ingress operation is unavailable")
	}
	return leaf, nil
}

func (ingress *V2CompatibilityIngress) Observe(ctx context.Context, step releasetransition.V2IngressStep) (releasetransition.Fingerprint, error) {
	leaf, err := ingress.requireLeaf(step.Kind)
	if err != nil {
		return "", err
	}
	return leaf.Observe(ctx, step)
}
func (ingress *V2CompatibilityIngress) Apply(ctx context.Context, step releasetransition.V2IngressStep) error {
	leaf, err := ingress.requireLeaf(step.Kind)
	if err != nil {
		return err
	}
	return leaf.Apply(ctx, step)
}
func (ingress *V2CompatibilityIngress) Verify(ctx context.Context, step releasetransition.V2IngressStep) error {
	leaf, err := ingress.requireLeaf(step.Kind)
	if err != nil {
		return err
	}
	return leaf.Verify(ctx, step)
}
