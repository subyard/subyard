package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/sshtrust"
)

func TestSSHTrustUsesTypedConsentAndDoesNotConsumeCommandPayload(t *testing.T) {
	for _, test := range []struct {
		name, input     string
		arguments       []string
		tty, automation bool
		want            error
	}{
		{name: "accept", input: "y\n", tty: true},
		{name: "default yes", input: "\n", tty: true},
		{name: "decline", input: "n\n", tty: true, want: domain.ErrOperationDeclined},
		{name: "EOF", tty: true, want: domain.ErrConfirmationRequired},
		{name: "pipe", input: "y\n", want: domain.ErrConfirmationRequired},
		{name: "automation", automation: true},
		{name: "flag", arguments: []string{"project", "--yes"}},
		{name: "payload flag", arguments: []string{"--", "some-command", "-y"}, want: domain.ErrConfirmationRequired},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			input := strings.NewReader(test.input)
			cli, err := New(Options{RepositoryRoot: repositoryRoot(t), Stdin: input, Stderr: &output, Environment: []string{"HOME=" + t.TempDir()}})
			if err != nil {
				t.Fatal(err)
			}
			cli.promptInputTerminal = func() bool { return test.tty }
			manager := cli.sshTrust(t.TempDir(), test.automation || sshTrustConsent(test.arguments))
			defer manager.Close()
			err = manager.Confirm(context.Background(), []sshtrust.Proposal{{Target: "yard-example", Namespace: "subyard-remote-example", File: "/state/known_hosts", Algorithm: "ssh-ed25519", Fingerprint: "SHA256:example"}})
			if !errors.Is(err, test.want) {
				t.Fatalf("confirmation=%v want=%v", err, test.want)
			}
			if test.tty && !strings.Contains(output.String(), "Proceed? [Y/n]") {
				t.Fatalf("wrong policy: %s", output.String())
			}
			if !test.tty && input.Len() != len(test.input) {
				t.Fatal("trust gate consumed command stdin")
			}
		})
	}
}
