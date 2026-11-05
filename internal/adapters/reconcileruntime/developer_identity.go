package reconcileruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
)

const developerIdentitySource = "config/identity/transition.py"

type developerAccount struct {
	User  string `json:"user"`
	UID   int    `json:"uid"`
	GID   int    `json:"gid"`
	Home  string `json:"home"`
	Shell string `json:"shell"`
}

type identityPackageReceipt struct {
	ID      string `json:"id"`
	Before  string `json:"before"`
	After   string `json:"after"`
	Check   string `json:"check"`
	Command string `json:"command"`
}

type developerIdentityCore struct {
	Boot     string                     `json:"boot"`
	Packages []identityPackageReceipt   `json:"packages"`
	Schema   int                        `json:"schema"`
	Machine  string                     `json:"machine"`
	Yard     map[string]string          `json:"yard"`
	Source   string                     `json:"source"`
	Old      developerAccount           `json:"old"`
	New      developerAccount           `json:"new"`
	Linger   bool                       `json:"linger"`
	Group    json.RawMessage            `json:"group"`
	Home     json.RawMessage            `json:"home"`
	Parent   json.RawMessage            `json:"parent"`
	Evidence string                     `json:"evidence"`
	Subids   json.RawMessage            `json:"subids"`
	Accounts json.RawMessage            `json:"accounts"`
	Receipts json.RawMessage            `json:"receipts"`
	Owners   map[string]json.RawMessage `json:"owners,omitempty"`
	Settings map[string]string          `json:"settings"`
	Sudo     bool                       `json:"sudo"`
	Listener map[string]bool            `json:"listener"`
}

type identityOwnerAccount struct {
	User  string `json:"user"`
	UID   int    `json:"uid"`
	GID   int    `json:"gid"`
	Home  string `json:"home"`
	Group string `json:"group"`
}

func validateIdentityOwnerRecipe(core developerIdentityCore, payload []byte) error {
	var envelope struct {
		Schema   int                  `json:"schema"`
		Source   string               `json:"source"`
		Original identityOwnerAccount `json:"original"`
		Target   identityOwnerAccount `json:"target"`
	}
	var group struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(payload, &envelope) != nil || json.Unmarshal(core.Group, &group) != nil || envelope.Schema != 1 || !profileRuntimeDigest.MatchString(envelope.Source) {
		return errors.New("invalid profile identity recipe")
	}
	old := identityOwnerAccount{core.Old.User, core.Old.UID, core.Old.GID, core.Old.Home, group.Name}
	wanted := identityOwnerAccount{core.New.User, core.New.UID, core.New.GID, core.New.Home, core.New.User}
	if envelope.Original != old || envelope.Target != wanted {
		return errors.New("profile identity recipe changed account scope")
	}
	return nil
}

// Owners print refusals as one fixed line that names their next step. Other
// handler stderr stays hidden because it can quote the files it was reading.
var identityOwnerRefusal = regexp.MustCompile(`(?m)^[A-Z][A-Za-z-]* identity: [ -~]{1,200}$`)

func (runtime Runtime) runIdentityOwner(ctx context.Context, definition profile.Definition, action string, arguments ...string) ([]byte, error) {
	output, stderr, err := runtime.runProfileRuntimeHandler(ctx, definition, action, arguments...)
	if err != nil {
		if refusal := identityOwnerRefusal.Find(stderr); refusal != nil {
			return nil, fmt.Errorf("%s: %w", refusal, err)
		}
		return nil, err
	}
	return output, nil
}

type developerIdentityObservation struct {
	hostRecovery bool
	Plan         *developerIdentityCore `json:"plan"`
	Pending      bool                   `json:"pending"`
	Phase        string                 `json:"phase"`
	CurrentUser  string                 `json:"current_user"`
	Fingerprint  string                 `json:"fingerprint"`
}

// DeveloperIdentityPlan is one bounded prerequisite of ordinary init. It never
// authorizes UID changes, arbitrary home targets or a second public operation.
type DeveloperIdentityPlan struct {
	core            developerIdentityCore
	baseFingerprint string
	source          string
	pending         bool
	phase           string
	old             Runtime
}

func (plan *DeveloperIdentityPlan) Binding() string {
	if plan == nil {
		return ""
	}
	payload, _ := json.Marshal(plan.core)
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return ""
	}
	return installedRuntimeDigest(value)
}

// RetiredIntegrationNotices observes pending integration retirements as the
// installed account, because the desired login does not exist before the rename.
func (plan *DeveloperIdentityPlan) RetiredIntegrationNotices(ctx context.Context) (retirements, warnings []string, err error) {
	return plan.old.RetiredIntegrationNotices(ctx)
}

func (plan *DeveloperIdentityPlan) Description() string {
	return fmt.Sprintf("rename developer %s to %s and home %s to %s; preserve UID %d, GID %d and retained data; briefly stop the managed SSH listener and account-bound profile services, interrupting their active sessions; existing developer processes must exit", plan.core.Old.User, plan.core.New.User, plan.core.Old.Home, plan.core.New.Home, plan.core.Old.UID, plan.core.Old.GID)
}

// DeferredIdentityRuntimes captures declared scope without observing a target
// account which does not exist until the identity prerequisite has completed.
func (runtime Runtime) DeferredIdentityRuntimes() (*ProfileRuntimePlan, error) {
	definitions, err := runtime.profileRuntimeDefinitions()
	if err != nil {
		return nil, err
	}
	plan := &ProfileRuntimePlan{definitions: definitions, approved: map[string]ports.RuntimeObservation{}, current: map[string]ports.RuntimeObservation{}, conditional: map[string]bool{}}
	for _, definition := range definitions {
		plan.conditional[definition.Runtime.ActivationID] = true
		observation := ports.RuntimeObservation{State: ports.RuntimeStateDeferred}
		plan.approved[definition.Runtime.ActivationID] = observation
		plan.current[definition.Runtime.ActivationID] = observation
	}
	return plan, nil
}

func (runtime Runtime) identityNative(ctx context.Context, mode string, request any) (developerIdentityObservation, error) {
	program, err := os.ReadFile(filepath.Join(runtime.RepositoryRoot, developerIdentitySource))
	if err != nil {
		return developerIdentityObservation{}, err
	}
	module, err := os.ReadFile(filepath.Join(runtime.RepositoryRoot, "config/identity/subids.py"))
	if err != nil {
		return developerIdentityObservation{}, err
	}
	accounts, err := os.ReadFile(filepath.Join(runtime.RepositoryRoot, "config/identity/accounts.py"))
	if err != nil {
		return developerIdentityObservation{}, err
	}
	prefix := "import types,sys\n_identity_subids=types.ModuleType('subids')\nexec(" + strconv.Quote(string(module)) + ",_identity_subids.__dict__)\nsys.modules['subids']=_identity_subids\n"
	prefix += "_identity_accounts=types.ModuleType('accounts')\nexec(" + strconv.Quote(string(accounts)) + ",_identity_accounts.__dict__)\nsys.modules['accounts']=_identity_accounts\n"
	program = append([]byte(prefix), program...)
	payload, err := json.Marshal(request)
	if err != nil {
		return developerIdentityObservation{}, err
	}
	result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, ports.InstanceExecRequest{Command: []string{"python3", "-B", "-c", string(program), mode}, Stdin: payload})
	if ctx.Err() != nil {
		return developerIdentityObservation{}, context.Cause(ctx)
	}
	if err != nil {
		var exit *ports.GuestExitError
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
			!errors.As(err, &exit) || exit.Code != result.ExitCode || exit.Code <= 0 {
			return developerIdentityObservation{}, err
		}
	}
	if result.ExitCode != 0 {
		var failure struct {
			Errors []string `json:"errors"`
		}
		decoder := json.NewDecoder(bytes.NewReader(result.Stdout))
		decoder.DisallowUnknownFields()
		valid := len(result.Stdout) <= 8192 && decoder.Decode(&failure) == nil && decoder.Decode(new(any)) == io.EOF && len(failure.Errors) > 0 && len(failure.Errors) <= 32
		for _, message := range failure.Errors {
			valid = valid && len(message) <= 512 && strings.TrimSpace(message) != "" && strings.IndexFunc(message, func(r rune) bool { return r < 32 || r == 127 }) < 0
		}
		if valid {
			return developerIdentityObservation{}, fmt.Errorf("developer identity: %s", strings.Join(failure.Errors, "; "))
		}
		return developerIdentityObservation{}, errors.New("developer account transition cannot verify native ownership")
	}
	if err != nil {
		return developerIdentityObservation{}, err
	}
	var observed developerIdentityObservation
	if len(result.Stdout) > 1024*1024 || json.Unmarshal(result.Stdout, &observed) != nil {
		return observed, errors.New("invalid developer identity observation")
	}
	return observed, nil
}

func (runtime Runtime) identityScope(ctx context.Context) (map[string]string, error) {
	scope := map[string]string{"name": runtime.Yard.YardName, "project": runtime.Yard.IncusProject, "instance": runtime.Yard.YardInstanceName}
	if runtime.Incus == nil {
		return scope, nil
	}
	instance, err := runtime.Incus.Instance(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName)
	if err != nil {
		return nil, err
	}
	uuid := instance.Config["volatile.uuid"]
	if len(uuid) != 36 || strings.Count(uuid, "-") != 4 || !domain.SafeName(uuid) {
		return nil, errors.New("native yard UUID unavailable for developer identity transition")
	}
	scope["uuid"] = uuid
	return scope, nil
}

func (runtime Runtime) developerIdentityInputs() (string, error) {
	definitions, err := runtime.profileRuntimeDefinitions()
	if err != nil {
		return "", err
	}
	sourceRuntime := runtime
	// Invocation transport and terminal variables are not durable authority.
	// Desired configuration is independently captured by the host continuation.
	sourceRuntime.Environment = nil
	sourceRuntime.PowerYards = nil
	inputs, err := sourceRuntime.profileRuntimeInputs(definitions)
	if err != nil {
		return "", err
	}
	executable, err := os.Open("/proc/self/exe")
	if err != nil {
		return "", err
	}
	defer executable.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(executable, (256<<20)+1))
	if err != nil {
		return "", err
	}
	if size > 256<<20 {
		return "", errors.New("identity engine source exceeds byte bound")
	}
	return installedRuntimeDigest(struct{ Inputs, Engine string }{inputs, fmt.Sprintf("%x", hash.Sum(nil))}), nil
}

func (runtime Runtime) PrepareDeveloperIdentity(ctx context.Context, recovery ...bool) (*DeveloperIdentityPlan, error) {
	if runtime.Executor == nil || runtime.Incus == nil {
		return nil, nil
	}
	instance, err := runtime.Incus.Instance(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName)
	if errors.Is(err, ports.ErrInstanceNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(instance.Status, "running") {
		return nil, nil
	}
	var applied AppliedIntegrationConfiguration
	if len(recovery) == 0 || !recovery[0] {
		applied, err = runtime.AppliedIntegrationConfiguration(ctx)
		if err != nil {
			return nil, err
		}
		if applied.Identity["DEV_USER"] == "" || applied.Identity["DEV_USER"] == runtime.devUser() && applied.Identity["DEV_UID"] == strconv.Itoa(runtime.Yard.DevUID) {
			return nil, nil
		}
	}
	source, err := runtime.developerIdentityInputs()
	if err != nil {
		return nil, err
	}
	scope, err := runtime.identityScope(ctx)
	if err != nil {
		return nil, err
	}
	request := map[string]any{"source": source, "yard": scope, "new_user": runtime.devUser(), "uid": runtime.Yard.DevUID}
	pending, err := runtime.identityNative(ctx, "pending", request)
	if err != nil {
		return nil, err
	}
	if pending.Plan != nil {
		if !pending.Pending {
			return nil, nil
		}
		applied = AppliedIntegrationConfiguration{Settings: pending.Plan.Settings, Identity: map[string]string{"DEV_USER": pending.Plan.Old.User, "DEV_UID": strconv.Itoa(pending.Plan.Old.UID)}}
	} else if applied.Identity == nil {
		applied, err = runtime.AppliedIntegrationConfiguration(ctx)
		if err != nil {
			return nil, err
		}
	}
	return runtime.prepareDeveloperIdentity(ctx, source, applied, pending)
}

func (runtime Runtime) prepareDeveloperIdentity(ctx context.Context, source string, applied AppliedIntegrationConfiguration, observed developerIdentityObservation) (*DeveloperIdentityPlan, error) {
	user := applied.Identity["DEV_USER"]
	uid, err := strconv.Atoi(applied.Identity["DEV_UID"])
	if user == "" {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("invalid applied developer UID")
	}
	if uid != runtime.Yard.DevUID {
		return nil, errors.New("developer UID migration is unsupported; retain the applied DEV_UID when renaming the account")
	}
	if user == runtime.devUser() && observed.Plan == nil {
		return nil, nil
	}
	old := runtime
	old.Yard.DevUser, old.Yard.DevUID = user, uid
	environment := runtimeEnvironment(runtime.Environment)
	if applied.Settings != nil {
		catalog, err := config.LoadCatalog(runtime.RepositoryRoot)
		if err != nil {
			return nil, err
		}
		environment = catalog.WithAppliedIntegrationInitSettings(environment, applied.Settings)
	}
	environment["DEV_USER"], environment["DEV_UID"] = user, strconv.Itoa(uid)
	old.Environment = nil
	for name, value := range environment {
		old.Environment = append(old.Environment, name+"="+value)
	}
	packages, err := old.developerIdentityPackages(runtime.devUser())
	if err != nil {
		return nil, err
	}
	if observed.Plan == nil {
		scope, scopeErr := runtime.identityScope(ctx)
		if scopeErr != nil {
			return nil, scopeErr
		}
		observed, err = runtime.identityNative(ctx, "observe", map[string]any{"source": source, "yard": scope, "old_user": user, "new_user": runtime.devUser(), "uid": uid, "packages": packages})
		if err != nil {
			return nil, err
		}
	}
	if observed.Plan == nil || observed.Plan.Old.User != user || observed.Plan.New.User != runtime.devUser() || observed.Plan.Old.UID != uid || observed.Plan.New.UID != uid || observed.Plan.Old.GID != observed.Plan.New.GID || observed.Plan.Source != source {
		return nil, errors.New("developer identity scope changed")
	}
	if installedRuntimeDigest(observed.Plan.Packages) != installedRuntimeDigest(packages) {
		return nil, errors.New("developer package identity source changed")
	}
	plan := &DeveloperIdentityPlan{core: *observed.Plan, baseFingerprint: observed.Fingerprint, source: source, pending: observed.Pending, phase: observed.Phase, old: old}
	plan.core.Settings = maps.Clone(applied.Settings)
	if plan.phase == "ready" || observed.hostRecovery {
		return plan, nil
	}
	if !plan.pending {
		plan.core.Owners = map[string]json.RawMessage{}
	}
	definitions, err := old.profileRuntimeDefinitions()
	if err != nil {
		return nil, err
	}
	var refusals []error
	for _, definition := range definitions {
		err := func() error {
			contract, err := definition.DeveloperIdentityContract()
			if err != nil {
				return err
			}
			if contract != "rebind" {
				observer := old
				if observed.Pending && observed.CurrentUser != "" && observed.CurrentUser != old.Yard.DevUser {
					observer.Yard.DevUser = observed.CurrentUser
					values := runtimeEnvironment(observer.Environment)
					values["DEV_USER"] = observed.CurrentUser
					observer.Environment = nil
					for name, value := range values {
						observer.Environment = append(observer.Environment, name+"="+value)
					}
				}
				state, err := observer.ObserveProfileRuntime(ctx, definition.Runtime.ActivationID)
				if err != nil {
					return err
				}
				if contract != "independent" && state.State != ports.RuntimeStateAbsent {
					return fmt.Errorf("profile %s has no developer identity transition contract", definition.Name)
				}
				return nil
			}
			output, err := old.runIdentityOwner(ctx, definition, "identity-observe", runtime.devUser(), source)
			if err != nil {
				return err
			}
			if err := validateIdentityOwnerRecipe(plan.core, output); err != nil {
				return err
			}
			var recipe any
			if json.Unmarshal(output, &recipe) != nil {
				return errors.New("invalid profile identity recipe")
			}
			if plan.pending {
				var before any
				if json.Unmarshal(plan.core.Owners[definition.Name], &before) != nil || installedRuntimeDigest(before) != installedRuntimeDigest(recipe) {
					return domain.ErrPlanStale
				}
			} else {
				plan.core.Owners[definition.Name] = json.RawMessage(output)
			}
			return nil
		}()
		if err != nil {
			refusals = append(refusals, fmt.Errorf("profile %s: %w", definition.Name, err))
		}
	}
	if len(refusals) != 0 {
		return nil, errors.Join(refusals...)
	}

	return plan, nil
}

func (runtime Runtime) BeginDeveloperIdentity(ctx context.Context, plan *DeveloperIdentityPlan) error {
	if plan == nil {
		return nil
	}
	source, err := runtime.developerIdentityInputs()
	if err != nil {
		return err
	}
	if source != plan.source {
		return domain.ErrPlanStale
	}
	scope, err := runtime.identityScope(ctx)
	if err != nil {
		return err
	}
	if !maps.Equal(scope, plan.core.Yard) {
		return domain.ErrPlanStale
	}
	observed, err := runtime.identityNative(ctx, "begin", map[string]any{"plan": plan.core, "expected": plan.baseFingerprint})
	if err != nil {
		return err
	}
	if observed.Plan == nil || !observed.Pending {
		return errors.New("developer identity authority was not published")
	}
	plan.phase = observed.Phase
	return nil
}

// AdvanceDeveloperIdentity changes only the approved account and owner recipes.
// Core receipt publication and the fresh ordinary init continuation follow it.
func (runtime Runtime) AdvanceDeveloperIdentity(ctx context.Context, plan *DeveloperIdentityPlan) error {
	if plan == nil {
		return nil
	}
	if plan.phase == "ready" {
		current, err := runtime.PrepareDeveloperIdentity(ctx, true)
		if err != nil {
			return err
		}
		if current == nil || current.Binding() != plan.Binding() {
			return domain.ErrPlanStale
		}
		return nil
	}
	if err := runtime.BeginDeveloperIdentity(ctx, plan); err != nil {
		return err
	}
	definitions, err := plan.old.profileRuntimeDefinitions()
	if err != nil {
		return err
	}
	for _, definition := range definitions {
		if _, ok := plan.core.Owners[definition.Name]; !ok {
			continue
		}
		output, err := plan.old.runIdentityOwner(ctx, definition, "identity-observe", plan.core.New.User, plan.source)
		if err != nil {
			return err
		}
		var observed, approved any
		if json.Unmarshal(output, &observed) != nil || json.Unmarshal(plan.core.Owners[definition.Name], &approved) != nil || installedRuntimeDigest(observed) != installedRuntimeDigest(approved) {
			return domain.ErrPlanStale
		}
	}
	if _, err := runtime.identityNative(ctx, "fence", map[string]any{"plan": plan.core}); err != nil {
		return err
	}
	if plan.phase == "prepared" || plan.phase == "quiesced" || plan.phase == "home" || plan.phase == "account" {
		for _, definition := range definitions {
			if _, ok := plan.core.Owners[definition.Name]; !ok {
				continue
			}
			if _, err := plan.old.runIdentityOwner(ctx, definition, "identity-apply", plan.source); err != nil {
				return err
			}
		}
		if plan.phase == "prepared" {
			observed, err := runtime.identityNative(ctx, "quiesced", map[string]any{"plan": plan.core})
			if err != nil {
				return err
			}
			plan.phase = observed.Phase
		}
	}
	if plan.phase == "quiesced" || plan.phase == "home" || plan.phase == "account" {
		observed, err := runtime.identityNative(ctx, "account", map[string]any{"plan": plan.core})
		if err != nil {
			return err
		}
		plan.phase = observed.Phase
	}
	if plan.phase != "account-ready" {
		return errors.New("developer identity has an unsupported continuation phase")
	}
	if _, err := runtime.identityNative(ctx, "receipts", map[string]any{"plan": plan.core}); err != nil {
		return err
	}
	for _, definition := range definitions {
		if _, ok := plan.core.Owners[definition.Name]; !ok {
			continue
		}
		if _, err := plan.old.runIdentityOwner(ctx, definition, "identity-apply", plan.source); err != nil {
			return err
		}
	}
	if err := runtime.verifyDeveloperIdentityPackages(ctx, plan); err != nil {
		return err
	}
	if _, err := runtime.identityNative(ctx, "commit", map[string]any{"plan": plan.core}); err != nil {
		return err
	}
	plan.phase = "ready"
	return nil
}

func (runtime Runtime) FinishDeveloperIdentity(ctx context.Context, plan *DeveloperIdentityPlan) error {
	if plan == nil {
		return nil
	}
	_, err := runtime.identityNative(ctx, "finish", map[string]any{"plan": plan.core})
	return err
}

// CompletionProof is retained only in the private host marker, so recovery can
// verify the retirement cut after the guest journal was durably removed.
func (plan *DeveloperIdentityPlan) CompletionProof() string {
	payload, _ := json.Marshal(plan.core)
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return ""
	}
	canonical, _ := json.Marshal(value)
	return string(canonical)
}

func (runtime Runtime) VerifyCompletedDeveloperIdentity(ctx context.Context, proof, binding string) error {
	var core developerIdentityCore
	decoder := json.NewDecoder(strings.NewReader(proof))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&core); err != nil {
		return fmt.Errorf("invalid identity completion proof: %w", err)
	}
	plan := DeveloperIdentityPlan{core: core}
	if plan.CompletionProof() != proof || plan.Binding() != binding || core.New.User != runtime.devUser() || core.New.UID != runtime.Yard.DevUID {
		return domain.ErrPlanStale
	}
	source, err := runtime.developerIdentityInputs()
	if err != nil {
		return err
	}
	scope, err := runtime.identityScope(ctx)
	if err != nil {
		return err
	}
	if source != core.Source || !maps.Equal(scope, core.Yard) {
		return domain.ErrPlanStale
	}
	_, err = runtime.identityNative(ctx, "finished", map[string]any{"plan": core})
	return err
}

// Derive both receipts from the same applied settings and immutable source. Only
// the login changes; unrelated desired settings never enter this recipe.
func (runtime Runtime) developerIdentityPackages(target string) ([]identityPackageReceipt, error) {
	before, err := runtime.integrationPackageArtifacts("inspect", runtime.SourceReader)
	if err != nil {
		return nil, err
	}
	next := runtime
	next.Yard.DevUser = target
	after, err := next.integrationPackageArtifacts("inspect", runtime.SourceReader)
	if err != nil {
		return nil, err
	}
	recipes := make([]identityPackageReceipt, 0, len(before))
	for i, entry := range before {
		check := runtime.environmentValue("AGENT_" + entry.ID + "_CHECK")
		command := runtime.environmentValue("AGENT_" + entry.ID + "_COMMAND")
		if check != "" && !safeCommand(check) || command != "" && !safeCommand(command) {
			return nil, errors.New("invalid applied package identity check")
		}
		recipes = append(recipes, identityPackageReceipt{entry.ID, entry.Digest, after[i].Digest, check, command})
	}
	return recipes, nil
}

func (runtime Runtime) verifyDeveloperIdentityPackages(ctx context.Context, plan *DeveloperIdentityPlan) error {
	for _, recipe := range plan.core.Packages {
		// Execute the declared applied probe under the retained numeric account,
		// with the canonical target HOME, before publishing its new receipt.
		commands := [][]string{}
		if recipe.Command != "" {
			commands = append(commands, []string{"sh", "-c", `command -v "$1" >/dev/null`, "subyard", recipe.Command})
		}
		if recipe.Check != "" {
			commands = append(commands, []string{recipe.Check})
		}
		for _, command := range commands {
			result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, ports.InstanceExecRequest{Command: command, User: uint32(plan.core.New.UID), Group: uint32(plan.core.New.GID), Environment: map[string]string{"HOME": plan.core.New.Home, "USER": plan.core.New.User, "LOGNAME": plan.core.New.User, "PATH": plan.core.New.Home + "/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}})
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			if err != nil || result.ExitCode != 0 {
				return fmt.Errorf("applied package %s is not ready under the target developer identity", recipe.ID)
			}
		}
	}
	return nil
}

// DeveloperIdentityObservationBinding pins an offline hint to this engine and
// the native instance. The hint is checked against fresh guest receipts on boot.
func (runtime Runtime) DeveloperIdentityObservationBinding(ctx context.Context) (string, string, error) {
	source, err := runtime.developerIdentityInputs()
	if err != nil {
		return "", "", err
	}
	scope, err := runtime.identityScope(ctx)
	if err != nil {
		return "", "", err
	}
	return source, scope["uuid"], nil
}
func (runtime Runtime) VerifiedAppliedDeveloperIdentity(ctx context.Context) (string, int, error) {
	applied, err := runtime.AppliedIntegrationConfiguration(ctx)
	if err != nil {
		return "", 0, err
	}
	user := applied.Identity["DEV_USER"]
	uid, err := strconv.Atoi(applied.Identity["DEV_UID"])
	if err != nil || user == "" || uid <= 0 {
		return "", 0, errors.New("applied developer identity unavailable")
	}
	_, err = runtime.identityNative(ctx, "applied", map[string]any{"user": user, "uid": uid})
	return user, uid, err
}

// PrepareDeveloperIdentityProof resumes the exact protected host marker at the
// cut before guest journal publication. It does not recapture owner intent.
func (runtime Runtime) PrepareDeveloperIdentityProof(ctx context.Context, proof, binding string) (*DeveloperIdentityPlan, error) {
	var core developerIdentityCore
	decoder := json.NewDecoder(strings.NewReader(proof))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&core); err != nil {
		return nil, err
	}
	saved := DeveloperIdentityPlan{core: core}
	if saved.CompletionProof() != proof || saved.Binding() != binding || core.New.User != runtime.devUser() || core.New.UID != runtime.Yard.DevUID {
		return nil, domain.ErrPlanStale
	}
	source, err := runtime.developerIdentityInputs()
	if err != nil {
		return nil, err
	}
	scope, err := runtime.identityScope(ctx)
	if err != nil {
		return nil, err
	}
	if core.Source != source || !maps.Equal(core.Yard, scope) {
		return nil, domain.ErrPlanStale
	}
	observed, err := runtime.identityNative(ctx, "recover-observe", map[string]any{"plan": core})
	if err != nil {
		return nil, err
	}
	if !observed.Pending {
		return nil, nil
	}
	observed.hostRecovery = observed.Phase == "host-prepared"
	if observed.hostRecovery {
		observed.Phase = "prepared"
	}
	return runtime.prepareDeveloperIdentity(ctx, source, AppliedIntegrationConfiguration{Settings: core.Settings, Identity: map[string]string{"DEV_USER": core.Old.User, "DEV_UID": strconv.Itoa(core.Old.UID)}}, observed)
}
