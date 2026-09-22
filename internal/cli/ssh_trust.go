package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/remotecontrol"
	"github.com/Subyard/Subyard/internal/adapters/transport"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerinventory"
	"github.com/Subyard/Subyard/internal/sshtrust"
	"golang.org/x/crypto/ssh"
)

func sshTrustConsent(arguments []string) bool {
	for _, argument := range arguments {
		if argument == "--" {
			break
		}
		if argument == "--yes" || argument == "-y" {
			return true
		}
	}
	return false
}

func (cli *CLI) sshTrust(dataHome string, assumeYes bool) *sshtrust.Manager {
	manager := &sshtrust.Manager{Environment: environmentList(cli.env, nil)}
	manager.Pin = func(target string) (*ownerinventory.SSHHostTrust, error) {
		connections, err := (ownerinventory.Connections{Root: filepath.Join(dataHome, "owner-inventory")}).ListReadOnly()
		if err != nil {
			return nil, err
		}
		for _, connection := range connections {
			if connection.Destination == target {
				return connection.Trust, nil
			}
		}
		return nil, nil
	}
	manager.Confirm = func(ctx context.Context, proposals []sshtrust.Proposal) error {
		consequences := make([]string, 0, len(proposals)*2+1)
		for _, proposal := range proposals {
			consequences = append(consequences,
				fmt.Sprintf("trust %s key %s for SSH target %s", proposal.Algorithm, proposal.Fingerprint, proposal.Target),
				fmt.Sprintf("add only this key as %s in %s", proposal.Namespace, proposal.File))
		}
		consequences = append(consequences, "verify the connection, then continue the requested command")
		assessment, err := cli.coreActions.Assess("ssh.trust", domain.ActionDelta{Changed: true, Consequences: consequences})
		if err != nil {
			return err
		}
		_, request, err := cli.coreActions.Resolve(assessment)
		if err != nil {
			return err
		}
		if assumeYes {
			return nil
		}
		prompt := cli.options.Prompt
		if prompt == nil {
			prompt = streamPrompt{input: cli.options.Stdin, output: cli.options.Stderr, interactive: cli.promptInputTerminal}
		}
		accepted, err := prompt.Confirm(ctx, *request)
		if err != nil {
			return err
		}
		if !accepted {
			return domain.ErrOperationDeclined
		}
		return nil
	}
	manager.Witness = func(ctx context.Context, program, target string, key ssh.PublicKey) error {
		loaded, err := cli.resolveContextWithYardSettings("default", "")
		if err != nil {
			return err
		}
		control := remotecontrol.Runtime{SSH: program,
			Home: loaded.Context.Paths.OperatorHome, ConfigHome: loaded.Context.Paths.ConfigHome,
			ConfigDir: loaded.Context.Paths.ConfigDir, DataHome: loaded.Context.Paths.DataHome,
			Environment: manager.Environment,
		}
		records, err := control.List(ctx)
		if err != nil {
			return err
		}
		for _, record := range records {
			if target != "yard-"+record.Spec.LegacyAlias {
				continue
			}
			if record.Spec.OwnerEndpoint == target {
				return fmt.Errorf("cyclic owner route for %s", target)
			}
			// The owner path passes the same trust gate, so a yard key is never
			// vouched for by an unverified jump host.
			keys, err := control.ScanYardKeys(ctx, record.Spec, record.SSHPort)
			if err != nil {
				return fmt.Errorf("verify yard SSH key through owner: %w", err)
			}
			material := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
			for _, scanned := range keys {
				if scanned.Material == material {
					return nil
				}
			}
			return fmt.Errorf("yard SSH key for %s does not match the trusted owner scan", target)
		}
		return nil
	}
	return manager
}

func (cli *CLI) sshArguments(ctx context.Context, target string, arguments []string) ([]string, error) {
	options, err := transport.SSHOptions(ctx, "ssh", target)
	return append(options, arguments...), err
}
