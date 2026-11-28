package reconcileruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

// The core receipt supplies an identity even when no coding integration has an
// inventory. Missing evidence is distinct from corrupt or foreign evidence.
func (runtime Runtime) appliedGuestIdentity(ctx context.Context) (map[string]string, error) {
	if !runtime.guestFactsAvailable() || runtime.Executor == nil {
		return nil, nil
	}
	program, err := os.ReadFile(filepath.Join(runtime.RepositoryRoot, guestFactsSource))
	if err != nil {
		return nil, err
	}
	result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, ports.InstanceExecRequest{
		Command: []string{"python3", "-B", "-c", "__name__ = 'subyard_identity_observer'\n" + string(program) + appliedGuestIdentityProjection},
		Stdin:   runtime.guestRuntimeFacts(),
	})
	if result.ExitCode == 10 && (err == nil || !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, errors.New("applied developer identity observation failed")
	}
	var identity map[string]string
	if len(result.Stdout) > 1024 || json.Unmarshal(result.Stdout, &identity) != nil || len(identity) != 2 {
		return nil, errors.New("invalid applied developer identity")
	}
	for _, name := range []string{"DEV_USER", "DEV_UID"} {
		value, ok := identity[name]
		if !ok {
			return nil, errors.New("missing applied developer identity")
		}
		if err := config.ValidateSetting(config.ScopeYard, name, value, false); err != nil {
			return nil, err
		}
	}
	return identity, nil
}

const appliedGuestIdentityProjection = `
core = Path('/var/lib/subyard/agent-skills/core.json')
if not FACTS.exists() and not FACTS.is_symlink():
    sys.exit(10)
value = read()
expected = snapshot(json.load(sys.stdin))
if value['status'] != 'current' or value['last_success'] is None or value['target']['yard'] != expected['yard']:
    raise ValueError('developer identity requires current guest facts for this yard')
if not core.exists() and not core.is_symlink():
    sys.exit(10)
safe(core)
info = core.lstat()
directory = core.parent.lstat()
if stat.S_IMODE(directory.st_mode) != 0o700 or directory.st_gid != 0 or not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600 or info.st_uid != 0 or info.st_gid != 0 or info.st_nlink != 1 or info.st_size > 16384:
    raise ValueError('unsafe applied developer identity receipt')
receipt = json.loads(core.read_bytes(), object_pairs_hook=unique_fields)
if not isinstance(receipt, dict) or set(receipt) != {'schema', 'home', 'extra', 'skills'} or type(receipt['schema']) is not int or receipt['schema'] != 1 or type(receipt['extra']) is not bool or not isinstance(receipt['skills'], dict) or not receipt['skills']:
    raise ValueError('invalid applied developer identity receipt')
import pwd
users = [user for user in pwd.getpwall() if user.pw_dir == receipt['home']]
if len(users) != 1 or receipt['home'] != '/home/' + users[0].pw_name or sum(user.pw_uid == users[0].pw_uid for user in pwd.getpwall()) != 1:
    raise ValueError('ambiguous applied developer identity')
print(json.dumps({'DEV_USER': users[0].pw_name, 'DEV_UID': str(users[0].pw_uid)}))
`

// AppliedEnvironmentProfiles reads the provisioner's protected successful
// selection. An explicitly empty applied list is different from missing proof.
// An unfinished refresh that keeps that selection, such as one stopped by a
// file collision, leaves it known, so recovery can repeat the refresh. One
// whose target changes the selection, including a profile operation recorded
// while the refresh was unfinished, refuses until init completes the refresh.
func (runtime Runtime) AppliedEnvironmentProfiles(ctx context.Context) ([]string, bool, error) {
	if !runtime.guestFactsAvailable() || runtime.Executor == nil {
		return nil, false, nil
	}
	program, err := os.ReadFile(filepath.Join(runtime.RepositoryRoot, guestFactsSource))
	if err != nil {
		return nil, false, err
	}
	result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, ports.InstanceExecRequest{
		Command: []string{"python3", "-B", "-c", "__name__ = 'subyard_profile_selection_observer'\n" + string(program) + appliedEnvironmentProfilesProjection},
		Stdin:   runtime.guestRuntimeFacts(),
	})
	if result.ExitCode == 10 && (err == nil || !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if result.ExitCode != 0 {
		return nil, false, errors.New("applied profile selection observation failed")
	}
	var selected []string
	if len(result.Stdout) > 16384 || json.Unmarshal(result.Stdout, &selected) != nil || selected == nil {
		return nil, false, errors.New("invalid applied profile selection")
	}
	seen := map[string]bool{}
	for _, name := range selected {
		if !domain.SafeName(name) || seen[name] {
			return nil, false, errors.New("invalid applied profile selection")
		}
		seen[name] = true
	}
	return selected, true, nil
}

const appliedEnvironmentProfilesProjection = `
if not FACTS.exists() and not FACTS.is_symlink():
    sys.exit(10)
value = read()
expected = snapshot(json.load(sys.stdin))
applied = value['last_success']
if applied is None or applied['yard'] != expected['yard']:
    raise ValueError('profile selection requires successful guest facts for this yard')
if value['status'] != 'current' and sorted(value['target']['selected_profiles']) != sorted(applied['selected_profiles']):
    raise ValueError('profile selection change is unfinished')
print(json.dumps(applied['selected_profiles']))
`

// ErrGuestFactsUnfinished reports guest facts left incomplete by an interrupted
// refresh; only a full refresh by init or release activation completes them.
var ErrGuestFactsUnfinished = errors.New("guest runtime facts refresh is unfinished; run yard init")

// RecordAppliedProfile updates one profile in the protected successful
// selection after that profile's runtime converged. Other entries keep their
// applied value: a profile operation applies only its own profile, and init
// still owns the whole selection snapshot. Missing facts stay with init and
// the release refresh; unfinished facts refuse until init rewrites them, and
// keep the requested change in their unfinished target.
func (runtime Runtime) RecordAppliedProfile(ctx context.Context, name string, selected bool) error {
	if !runtime.guestFactsAvailable() || runtime.Executor == nil {
		return nil
	}
	if !domain.SafeName(name) {
		return errors.New("invalid applied profile name")
	}
	program, err := os.ReadFile(filepath.Join(runtime.RepositoryRoot, guestFactsSource))
	if err != nil {
		return err
	}
	var target map[string]any
	if err := json.Unmarshal(runtime.guestRuntimeFacts(), &target); err != nil {
		return err
	}
	request, err := json.Marshal(map[string]any{"yard": target["yard"], "profile": name, "selected": selected})
	if err != nil {
		return err
	}
	result, err := runtime.Executor.Exec(ctx, runtime.Yard.IncusProject, runtime.Yard.YardInstanceName, ports.InstanceExecRequest{
		Command: []string{"python3", "-B", "-c", "__name__ = 'subyard_profile_selection_recorder'\n" + string(program) + recordAppliedProfileProjection},
		Stdin:   request,
	})
	if result.ExitCode == 10 && (err == nil || !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)) {
		return nil
	}
	if result.ExitCode == 12 && (err == nil || !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)) {
		// An interrupted refresh or record must not look recorded; init rewrites the snapshot.
		return ErrGuestFactsUnfinished
	}
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return errors.New("applied profile selection record failed")
	}
	return nil
}

const recordAppliedProfileProjection = `
if not FACTS.exists() and not FACTS.is_symlink():
    sys.exit(10)
request = json.load(sys.stdin)
value = read()
applied = value['last_success']
if value['target']['yard'] != request['yard']:
    sys.exit(10)
name = request['profile']
def record(base):
    profiles = [item for item in base['selected_profiles'] if item != name]
    if request['selected']:
        profiles = list(base['selected_profiles']) if name in base['selected_profiles'] else profiles + [name]
    return dict(base, selected_profiles=profiles)
if value['status'] != 'current':
    # Keep the change in the unfinished target, so the selection reader refuses
    # a stale successful selection instead of reporting it as applied.
    pending = record(value['target'])
    if pending != value['target']:
        apply('begin', pending)
    sys.exit(12)
recorded = record(applied)
if recorded != applied:
    apply('begin', recorded)
    apply('commit', recorded)
`
