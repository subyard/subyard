package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/ownerapi"
)

// profileProbeTimeout bounds one yard's read-only profile observation.
const profileProbeTimeout = 2 * time.Minute

// pendingProfile is a selected profile whose owning provision check reports
// changes for the installed release.
type pendingProfile struct {
	yard    string
	profile string
}

// offerPendingProfileProvision runs after a successful release activation or
// migration. The installed runtime observes the selected profiles of every
// local yard through its read-only owning checks. Interactive runs offer each
// pending profile through the ordinary scoped provision plan, which keeps its
// own confirmation, guards and verification. It never changes the outcome of
// the completed release operation.
func (cli *CLI) offerPendingProfileProvision(ctx context.Context, execution *releaseExecution, assumeYes bool) {
	launcher := execution.launcher
	if launcher == "" {
		launcher = execution.prepared.ActiveLauncher
	}
	if launcher == "" || execution.configHome == "" || cli.profileOfferSuppressed {
		return
	}
	runtimeRoot := filepath.Dir(filepath.Dir(launcher))
	configDir := filepath.Join(runtimeRoot, "config")
	yards, err := config.YardNames(configDir, execution.configHome)
	if err != nil {
		return
	}
	// Children observe persisted settings for an explicit yard, like activation repair.
	environment := cli.freshMigrationEnvironment(cli.baseEnv, runtimeRoot)
	delete(environment, "YARD_ENGINE_PATH")
	delete(environment, "ASSUME_YES")
	pending := []pendingProfile{}
	for _, yard := range yards {
		var output bytes.Buffer
		probe, cancel := context.WithTimeout(ctx, profileProbeTimeout)
		command := exec.CommandContext(probe, launcher, "-Y", yard, "profile", "list", "--json")
		command.Env = environmentList(environment, map[string]string{"SUBYARD_OPERATION_ID": newOperationID()})
		command.Stdin, command.Stdout, command.Stderr = strings.NewReader(""), &output, io.Discard
		err := command.Run()
		cancel()
		if err != nil {
			continue
		}
		var list ownerapi.ProfileList
		if json.Unmarshal(output.Bytes(), &list) != nil {
			continue
		}
		for _, entry := range list.Profiles {
			if entry.ProvisionPending() {
				pending = append(pending, pendingProfile{yard: yard, profile: entry.Name})
			}
		}
	}
	if len(pending) == 0 || ctx.Err() != nil {
		return
	}
	output := cli.options.Stdout
	interactive := !assumeYes && cli.env["ASSUME_YES"] != "1" &&
		cli.promptInputTerminal != nil && cli.promptInputTerminal()
	for _, item := range pending {
		fmt.Fprintf(output, "\nProfile %s in yard %s is selected but not provisioned for this release.\n", item.profile, item.yard)
		if !interactive {
			fmt.Fprintf(output, "  Provision it with: %s\n", strings.Join(append([]string{cli.options.Program}, yardArguments(item.yard, "provision", item.profile)...), " "))
			continue
		}
		// Not bound to ctx: a terminal interrupt reaches provision itself, which
		// owns its interruption cleanup; this process only waits for it.
		command := exec.Command(launcher, "-Y", item.yard, "provision", item.profile)
		command.Env = environmentList(environment, map[string]string{"SUBYARD_OPERATION_ID": newOperationID()})
		command.Stdin, command.Stdout, command.Stderr = cli.options.Stdin, cli.options.Stdout, cli.options.Stderr
		if command.Run() != nil {
			fmt.Fprintf(output, "  Profile %s was not provisioned; the next yard update or yard migrate offers it again.\n", item.profile)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func yardArguments(yard string, arguments ...string) []string {
	if yard == "" || yard == "default" {
		return arguments
	}
	return append([]string{"-Y", yard}, arguments...)
}
