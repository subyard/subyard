package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"

	"github.com/Subyard/Subyard/internal/adapters/credentialruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
)

type initProfileSetup struct {
	definition                profile.Definition
	configHome, path, keyPath string
	before                    config.PersistentFileSnapshot
	values                    map[string]any
	runtime                   *credentialruntime.Runtime
	key                       *credentialruntime.Prepared
}
type initProfileSet struct {
	root        string
	definitions []profile.Definition
	items       []*initProfileSetup
}

func (cli *CLI) prepareInitProfiles(ctx context.Context, execution *initExecution, arguments []string) (*initProfileSet, error) {
	if execution.mode != initReconcile || execution.loaded.Context.AccessKind != domain.AccessLocal || cli.releaseTransitionChild {
		return nil, nil
	}
	definitions, err := profile.Load(cli.options.RepositoryRoot)
	if err != nil {
		return nil, err
	}
	set := &initProfileSet{root: cli.options.RepositoryRoot, definitions: definitions}
	for _, definition := range definitions {
		if definition.Setup == nil || !definition.Selected(execution.loaded.Context.YardName, execution.loaded.Environment) {
			continue
		}
		setup, err := cli.prepareInitProfile(ctx, execution, arguments, definition)
		if err != nil {
			return nil, err
		}
		if setup != nil {
			set.items = append(set.items, setup)
		}
	}
	if len(set.items) == 0 {
		return nil, nil
	}
	return set, nil
}

func (cli *CLI) prepareInitProfile(ctx context.Context, execution *initExecution, arguments []string, definition profile.Definition) (*initProfileSetup, error) {
	loaded := execution.loaded
	schema := definition.Setup
	setup := &initProfileSetup{definition: definition, configHome: loaded.Context.Paths.ConfigHome, values: map[string]any{}}
	setup.path = filepath.Join(setup.configHome, schema.ConfigFile)
	var err error
	setup.before, err = readInitSelectionSnapshot(setup.configHome, setup.path)
	if err != nil {
		return nil, err
	}
	setup.runtime, err = cli.credentialRuntime(loaded)
	if err != nil {
		return nil, err
	}
	defaultKey, err := setup.runtime.ConsumerPath(schema.Consumer, schema.Zone)
	if err != nil {
		return nil, err
	}
	setup.keyPath = defaultKey
	if setup.before.Exists {
		setup.values, err = schema.Decode(setup.before.Content)
		if err != nil || setup.before.Identity.Mode&0o777 != 0o600 && setup.before.Identity.Mode&0o777 != 0o400 {
			fmt.Fprintf(cli.options.Stdout, "  [ .. ] %s settings are invalid or unprotected; repair %s on the owner host. Existing settings were kept.\n", definition.Name, schema.ConfigFile)
			return nil, nil
		}
		if override, _ := setup.values[schema.KeyOverrideField].(string); override != "" {
			setup.keyPath = override
		}
		if protectedSetupKeyExists(setup.keyPath) {
			if err := setup.runtime.ValidateConsumerFile(schema.Consumer, schema.Zone, setup.keyPath); err != nil {
				fmt.Fprintf(cli.options.Stdout, "  [ .. ] Existing %s credential is invalid; restore it or rotate it with yard keys rotate. Existing settings were kept.\n", definition.Name)
			}
			return nil, nil
		}
		if setup.keyPath != defaultKey {
			fmt.Fprintf(cli.options.Stdout, "  [ .. ] %s credential is unavailable; restore the configured %s on the owner host.\n", definition.Name, schema.KeyOverrideField)
			return nil, nil
		}
	}
	if _, err := os.Lstat(defaultKey); err == nil && !protectedSetupKeyExists(defaultKey) {
		fmt.Fprintf(cli.options.Stdout, "  [ .. ] Existing %s credential has unsafe permissions or type; repair it on the owner host before setup. The file was kept.\n", definition.Name)
		return nil, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if !cli.promptInputTerminal() || keysAssumeYes(arguments) || cli.env["ASSUME_YES"] == "1" || cli.baseEnv["SUBYARD_SG_REEXEC"] == "1" {
		fmt.Fprintf(cli.options.Stdout, "  [ .. ] %s is not configured. Run yard -Y %s init interactively on the owner host to set it up.\n", definition.Name, loaded.Context.YardName)
		return nil, nil
	}
	fmt.Fprintf(cli.options.Stdout, "\n%s (optional)\n", schema.Title)
	for _, instruction := range schema.Instructions {
		fmt.Fprintln(cli.options.Stdout, instruction)
	}
	if !setup.before.Exists {
		for _, field := range schema.Fields {
			for {
				value, err := cli.readProfileSetupLine(ctx, field.Label+" (Enter to set up later): ")
				if err != nil || value == "" {
					return nil, err
				}
				parsed, err := field.Parse(value)
				if err == nil {
					setup.values[field.Name] = parsed
					break
				}
				fmt.Fprintln(cli.options.Stdout, err)
			}
		}
	}
	if !protectedSetupKeyExists(defaultKey) {
		existing, err := setup.runtime.ConsumerCredentialID(ctx, schema.Consumer, schema.Zone)
		if err != nil {
			return nil, err
		}
		source := ""
		for {
			if existing == "" {
				source, err = cli.readProfileSetupLine(ctx, "Credential file path on this host (Enter to set up later): ")
				if err != nil || source == "" {
					return nil, err
				}
				if strings.HasPrefix(source, "~/") {
					source = filepath.Join(loaded.Context.Paths.OperatorHome, source[2:])
				}
			}
			key, err := setup.runtime.PrepareSetupCredential(ctx, credentialruntime.SetupCredentialOptions{Consumer: schema.Consumer, Zone: schema.Zone, Label: schema.Label, Source: source})
			if err == nil {
				setup.key = &key
				break
			}
			if existing != "" {
				return nil, err
			}
			fmt.Fprintln(cli.options.Stdout, err)
		}
	} else {
		fmt.Fprintln(cli.options.Stdout, "Reusing the existing protected credential on this host.")
	}
	return setup, nil
}

func protectedSetupKeyExists(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && (info.Mode().Perm() == 0o600 || info.Mode().Perm() == 0o400)
}

// Read one line without consuming the next central confirmation answer.
func (cli *CLI) readProfileSetupLine(ctx context.Context, label string) (string, error) {
	fmt.Fprint(cli.options.Stdout, label)
	var line strings.Builder
	var one [1]byte
	for line.Len() <= 4096 {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if _, err := io.ReadFull(cli.options.Stdin, one[:]); err != nil {
			return "", errors.New("profile setup input ended; no setup was applied, rerun init to continue")
		}
		if one[0] == '\n' {
			return strings.TrimSpace(line.String()), nil
		}
		if one[0] != '\r' && (one[0] < 32 || one[0] == 127) {
			return "", errors.New("invalid control character in profile setup input")
		}
		line.WriteByte(one[0])
	}
	return "", errors.New("profile setup input is too long")
}

func (set *initProfileSet) check() error {
	if set == nil {
		return nil
	}
	current, err := profile.Load(set.root)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, set.definitions) {
		return fmt.Errorf("%w: profile declarations changed", domain.ErrPlanStale)
	}
	for _, setup := range set.items {
		if err := setup.check(); err != nil {
			return err
		}
	}
	return nil
}
func (setup *initProfileSetup) check() error {
	current, err := readInitSelectionSnapshot(setup.configHome, setup.path)
	if err != nil {
		return err
	}
	if !sameConfigAuthoringSnapshot(current, setup.before) || current.Identity != setup.before.Identity {
		return fmt.Errorf("%w: profile settings changed; rerun init", domain.ErrPlanStale)
	}
	return nil
}
func (set *initProfileSet) consequences() []string {
	if set == nil {
		return nil
	}
	result := []string{}
	for _, setup := range set.items {
		result = append(result, "configure "+setup.definition.Name+" on this owner host")
		for _, field := range setup.definition.Setup.Fields {
			result = append(result, fmt.Sprintf("%s: %v", field.Label, setup.values[field.Name]))
		}
		if setup.key != nil {
			result = append(result, setup.key.Consequences...)
		}
	}
	return result
}
func (set *initProfileSet) apply(ctx context.Context, execution *initExecution, output io.Writer) error {
	if set == nil {
		return nil
	}
	if err := set.check(); err != nil {
		return err
	}
	for _, setup := range set.items {
		if setup.key != nil {
			// Persist approved credentials before Incus can exec a fresh group session.
			for _, stage := range application.InitStages(execution.loaded.Context) {
				if stage.ID != ports.ReconcileStageKeys {
					continue
				}
				if err := (application.Reconciler{Stages: []application.ReconcileStage{stage}, Runner: execution.platform, Reporter: initReporter{output: output}}).Apply(ctx); err != nil {
					return err
				}
			}
			if err := setup.key.Execute(ctx); err != nil {
				return fmt.Errorf("set up profile credential: %w", err)
			}
		}
		schema := setup.definition.Setup
		if err := setup.runtime.ValidateConsumerFile(schema.Consumer, schema.Zone, setup.keyPath); err != nil {
			return fmt.Errorf("verify profile credential: %w", err)
		}
		if !setup.before.Exists {
			payload, err := json.MarshalIndent(setup.values, "", "  ")
			if err != nil {
				return err
			}
			if err := config.CreatePersistentFile(setup.configHome, setup.path, append(payload, '\n')); err != nil {
				return err
			}
		} else if err := setup.check(); err != nil {
			return err
		}
		fmt.Fprintf(output, "  [ ok ] %s settings and credential are ready on this owner host.\n", setup.definition.Name)
		if schema.Followup != "" {
			fmt.Fprintln(output, "  [ .. ] "+schema.Followup)
		}
	}
	return nil
}
