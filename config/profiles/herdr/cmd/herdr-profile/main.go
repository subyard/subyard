package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Subyard/Subyard/internal/domain"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "Herdr:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 && args[0] == "keys" {
		return manageKeys(args[1:])
	}
	if len(args) > 0 && args[0] == "host-keys" {
		return hostKeys(args[1:])
	}
	path := fmt.Sprintf("/run/user/%d/herdr-core/default.sock", os.Getuid())
	if len(args) > 0 && (args[0] == "sync" || args[0] == "hook") {
		restore := len(args) == 2 && args[0] == "sync" && args[1] == "--restore"
		if len(args) != 1 && !restore {
			return errors.New("invalid project sync arguments")
		}
		return syncProjects(path, "/srv/workspaces", restore, args[0] == "hook")
	}
	if len(args) != 1 {
		return errors.New("expected a guest operation")
	}
	switch args[0] {
	case "discover":
		return discoverCheckouts(path, "/srv/workspaces")
	case "registration-ready":
		return registrationReady(path, "/srv/workspaces")
	case "observe", "status":
		return observe(path, args[0] == "status")
	case "stop":
		core, err := readCore(path)
		if err != nil || core == nil {
			return err
		}
		return stop(path, core.PID)
	default:
		return errors.New("unknown guest operation")
	}
}

func stop(path string, pid int) error {
	if err := call(path, "server.stop", nil, nil); err != nil {
		return err
	}
	var probeErr error
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		core, err := readCore(path)
		// Core can close an accepted probe while its shutdown is still in progress.
		if errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
			probeErr = err
			continue
		}
		if err != nil || core == nil {
			return err
		}
		probeErr = nil
		if core.PID != pid {
			return errors.New("Herdr Core restarted while stopping")
		}
	}
	if probeErr != nil {
		return fmt.Errorf("Herdr Core shutdown could not be verified: %w", probeErr)
	}
	return errors.New("Herdr Core is still running")
}

func observe(socket string, human bool) error {
	core, err := readCore(socket)
	if !human {
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(core)
	}
	home, homeErr := os.UserHomeDir()
	var pathErr error
	if homeErr == nil {
		pathErr = reportExecutables(home, os.Getenv("PATH"))
	}
	if core == nil && err == nil {
		fmt.Println("Core: stopped")
	}
	if core != nil {
		fmt.Printf("Core: %s (PID %d, %d live terminals)\n", core.Version, core.PID, core.Live)
		if minimum := os.Getenv("SUBYARD_HERDR_MINIMUM"); minimum != "" && versionBelow(core.Version, minimum) {
			fmt.Printf("Core is below the minimum %s. Use yard herdr up to repair it.\n", minimum)
		}
		for _, holder := range core.Holders {
			if !holder.Current {
				fmt.Printf("Old holder: PID %d, %d live terminals\n", holder.PID, holder.Terminals)
			}
		}
		if core.CleanStart == "yes" {
			fmt.Println("Clean startup marker: yes")
		} else {
			fmt.Printf("Clean startup marker: %s. Use yard herdr restart to refresh the login environment.\n", core.CleanStart)
		}
	}
	return domain.CollectRefusals(err, homeErr, pathErr, reportPending())
}

func versionBelow(candidate, floor string) bool {
	var actual, minimum [3]int
	n, _ := fmt.Sscanf(candidate, "%d.%d.%d", &actual[0], &actual[1], &actual[2])
	m, _ := fmt.Sscanf(floor, "%d.%d.%d", &minimum[0], &minimum[1], &minimum[2])
	if n != 3 || m != 3 {
		return false
	}
	for i := range actual {
		if actual[i] != minimum[i] {
			return actual[i] < minimum[i]
		}
	}
	return false
}

func reportExecutables(home, path string) error {
	canonical := filepath.Join(home, ".local/bin/herdr-core")
	link := filepath.Join(home, ".local/bin/hc")
	target, err := os.Readlink(link)
	var linkErr error
	if err == nil && target != "herdr-core" {
		linkErr = errors.New("the hc symlink points outside the managed binary")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		linkErr = errors.New("hc is not the managed symlink")
	}
	var copies []error
	for _, dir := range filepath.SplitList(path) {
		if !filepath.IsAbs(dir) {
			continue
		}
		for _, name := range []string{"herdr-core", "hc"} {
			candidate := filepath.Join(dir, name)
			if candidate == canonical || candidate == link {
				continue
			}
			target, err := filepath.EvalSymlinks(candidate)
			if err != nil || target == canonical {
				continue
			}
			if info, err := os.Stat(target); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
				copies = append(copies, fmt.Errorf("additional Herdr executable at %s", strings.ToValidUTF8(candidate, "?")))
			}
		}
	}
	return domain.CollectRefusals(append(copies, linkErr)...)
}
