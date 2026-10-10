package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/credentialruntime"
)

// Synchronized fields travel inside the credential, never ordinary config sync.
// Matching legacy JSON delegates to the credential after verified migration;
// explicit local key overrides remain unchanged.
func (cli *CLI) prepareSyncedInitProfile(ctx context.Context, execution *initExecution, arguments []string, setup *initProfileSetup) (*initProfileSetup, error) {
	schema := setup.definition.Setup
	if setup.before.Exists {
		values, err := schema.Decode(setup.before.Content)
		if err != nil || setup.before.Identity.Mode&0o777 != 0o600 && setup.before.Identity.Mode&0o777 != 0o400 {
			fmt.Fprintf(cli.options.Stdout, "  [ .. ] %s settings are invalid or unprotected; repair %s on the owner host. Existing settings were kept.\n", setup.definition.Name, schema.ConfigFile)
			return nil, nil
		}
		setup.values = values
		if values["use_credential_settings"] == true {
			setup.values = map[string]any{}
		}
		if override, _ := values[schema.KeyOverrideField].(string); override != "" {
			if err := setup.runtime.ValidateConsumerFile(schema.Consumer, schema.Zone, override); err != nil {
				fmt.Fprintf(cli.options.Stdout, "  [ .. ] %s local credential override is unavailable or invalid; restore it on the owner host. Existing settings were kept.\n", setup.definition.Name)
			}
			return nil, nil
		}
	}
	if _, err := os.Lstat(setup.keyPath); err == nil {
		binding, err := setup.runtime.ConsumerFileBinding(setup.keyPath)
		if err != nil {
			return nil, err
		}
		setup.keyBinding = binding
		fields, err := setup.runtime.ConsumerSettings(schema.Consumer, schema.Zone, setup.keyPath)
		if err != nil {
			fmt.Fprintf(cli.options.Stdout, "  [ .. ] Existing %s connection is invalid or unprotected; repair it on the owner host. Existing settings were kept.\n", setup.definition.Name)
			return nil, nil
		}
		if fields != nil {
			// A synchronized connection is already complete, including on a peer
			// with no local setup JSON. Do not create a second copy of its fields.
			if !setup.before.Exists || len(setup.values) == 0 {
				return nil, nil
			}
			// Adopt matching legacy JSON under the same reviewed init operation.
			want, err := schema.SharedSettings(setup.values)
			if err != nil {
				return nil, err
			}
			if !reflect.DeepEqual(fields, want) {
				fmt.Fprintf(cli.options.Stdout, "  [ .. ] Local %s settings conflict with the synchronized connection. Existing settings were kept.\n", setup.definition.Name)
				return nil, nil
			}
			return setup, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	existing, err := setup.runtime.ConsumerCredentialID(ctx, schema.Consumer, schema.Zone)
	if err != nil {
		return nil, err
	}
	source := ""
	if existing == "" {
		for _, candidate := range []string{setup.keyPath, setup.runtime.LegacyConsumerPath(schema.Consumer)} {
			if candidate != "" && protectedSetupKeyExists(candidate) {
				if err := setup.runtime.ValidateConsumerFile(schema.Consumer, schema.Zone, candidate); err != nil {
					return nil, err
				}
				source = candidate
				break
			}
		}
	}
	interactive := cli.promptInputTerminal() && !keysAssumeYes(arguments) && cli.env["ASSUME_YES"] != "1"
	legacyFieldsNeeded := protectedSetupKeyExists(setup.keyPath) || protectedSetupKeyExists(setup.runtime.LegacyConsumerPath(schema.Consumer))
	if !setup.before.Exists && (existing == "" || legacyFieldsNeeded) {
		if !interactive {
			fmt.Fprintf(cli.options.Stdout, "  [ .. ] %s is not configured. Run yard -Y %s init interactively on the owner host to set it up.\n", setup.definition.Name, execution.loaded.Context.YardName)
			return nil, nil
		}
		fmt.Fprintf(cli.options.Stdout, "\n%s (optional)\n", schema.Title)
		for _, instruction := range schema.Instructions {
			fmt.Fprintln(cli.options.Stdout, instruction)
		}
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
	for {
		if source == "" && existing == "" {
			if !interactive {
				fmt.Fprintf(cli.options.Stdout, "  [ .. ] %s credential is unavailable; restore its protected PEM or run init interactively on the owner host.\n", setup.definition.Name)
				return nil, nil
			}
			source, err = cli.readProfileSetupLine(ctx, "Credential file path on this host (Enter to set up later): ")
			if err != nil || source == "" {
				return nil, err
			}
			if strings.HasPrefix(source, "~/") {
				source = filepath.Join(execution.loaded.Context.Paths.OperatorHome, source[2:])
			}
		}
		var fields map[string]any
		if len(setup.values) != 0 {
			fields = setup.values
		}
		key, err := setup.runtime.PrepareSetupCredential(ctx, credentialruntime.SetupCredentialOptions{Consumer: schema.Consumer, Zone: schema.Zone, Label: schema.Label, Source: source, Settings: fields})
		if err == nil {
			setup.key = &key
			return setup, nil
		}
		if existing != "" || !interactive {
			return nil, err
		}
		fmt.Fprintln(cli.options.Stdout, err)
		source = ""
	}
}
