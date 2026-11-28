package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Subyard/Subyard/internal/domain"
)

const discoveryReportLimit = 8 << 20

// discoveryProgram runs the shared checkout discovery CLI that the profile installs.
var discoveryProgram = []string{"/usr/bin/python3", "-B", "-E", "-s", "/usr/local/libexec/subyard/herdr-discovery/checkouts.py"}

// checkout is the desired space of one project checkout.
type checkout struct {
	Name, Folder string
}

// inventory holds the desired checkouts of the present projects and the known
// checkouts whose absence discovery proved. Problems are refusals that sync
// reports after applying the rest: a partial scan proves no absence, and a
// project in Kept keeps the records of checkouts it no longer lists.
type inventory struct {
	Checkouts map[string]checkout
	Gone      map[string]bool
	Kept      map[string]bool
	Partial   bool
	Problems  []string
}

func (inventory inventory) refusal() error {
	refusals := make([]error, 0, len(inventory.Problems))
	for _, problem := range inventory.Problems {
		refusals = append(refusals, errors.New(problem))
	}
	return domain.CollectRefusals(refusals...)
}

// retains reports whether a checkout that is neither listed nor proven absent
// keeps its record.
func (inventory inventory) retains(key string) bool {
	id, _ := splitKey(key)
	return !inventory.Gone[key] && (inventory.Partial || inventory.Kept[id])
}

// A checkout key is the project ID for src and "ID/path" for another checkout,
// where path is relative to the project directory: "src/private" or "wt-feature".
func checkoutKey(projectID, path string) string {
	if path == "" {
		return projectID
	}
	return projectID + "/" + path
}

func splitKey(key string) (string, string) {
	id, path, _ := strings.Cut(key, "/")
	return id, path
}

func validCheckoutPath(path string) bool {
	return path != "src" && filepath.IsLocal(path) && filepath.Clean(path) == path && utf8.ValidString(path) &&
		!strings.ContainsAny(path, "\x00\r\n") && (strings.HasPrefix(path, "src/") || !strings.Contains(path, "/"))
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	if buffer.Len()+len(data) > buffer.limit {
		return 0, errors.New("output limit exceeded")
	}
	return buffer.Buffer.Write(data)
}

func runDiscovery(input []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, discoveryProgram[0], append(slices.Clone(discoveryProgram[1:]), args...)...)
	command.Stdin = bytes.NewReader(input)
	stdout, stderr := &limitedBuffer{limit: discoveryReportLimit}, &limitedBuffer{limit: 64 << 10}
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Run(); err != nil {
		lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
		if last := strings.ToValidUTF8(lines[len(lines)-1], "?"); last != "" {
			return nil, fmt.Errorf("checkout discovery failed: %w: %s", err, last)
		}
		return nil, fmt.Errorf("checkout discovery failed: %w", err)
	}
	return stdout.Bytes(), nil
}

func discoveryState(dir string) string { return filepath.Join(dir, "discovery") }

// advanceDiscovery runs one bounded, resumable portion of the nested checkout scan.
func advanceDiscovery(workspaces, dir string) error {
	output, err := runDiscovery(nil, "advance", "--workspaces", workspaces, "--state", discoveryState(dir))
	if err != nil {
		return err
	}
	var report struct{ Errors []string }
	if json.Unmarshal(output, &report) != nil {
		return errors.New("checkout discovery returned an invalid report")
	}
	return discoveryErrors(report.Errors)
}

func discoveryErrors(messages []string) error {
	return inventory{Problems: discoveryProblems(messages)}.refusal()
}

func discoveryProblems(messages []string) []string {
	problems := make([]string, 0, len(messages))
	for _, message := range messages {
		problems = append(problems, "checkout discovery: "+strings.ToValidUTF8(message, "?"))
	}
	return problems
}

func knownFolder(workspaces, key string) string {
	id, path := splitKey(key)
	return filepath.Join(workspaces, id, path)
}

// scanCheckouts reports src, every Git root below it and the Git roots beside it
// for each project, and which known checkouts are proven absent. Its error means
// no inventory; findings that leave other checkouts usable are Problems.
func scanCheckouts(workspaces, dir string, known []string) (inventory, error) {
	folders := make([]string, 0, len(known))
	for _, key := range known {
		folders = append(folders, knownFolder(workspaces, key))
	}
	input, _ := json.Marshal(folders)
	output, err := runDiscovery(input, "roots", "--siblings", "--workspaces", workspaces, "--state", discoveryState(dir))
	if err != nil {
		return inventory{}, err
	}
	var report struct {
		Projects []struct {
			ProjectID, Name, Root string
			Roots                 []struct{ Path, Kind string }
		}
		Absent, Errors []string
	}
	if json.Unmarshal(output, &report) != nil || (report.Projects == nil && len(report.Errors) == 0) {
		return inventory{}, errors.New("checkout discovery returned an invalid report")
	}
	// A partial scan still lists present checkouts but proves no absence.
	result := inventory{Checkouts: make(map[string]checkout), Gone: make(map[string]bool), Kept: make(map[string]bool),
		Partial: len(report.Errors) > 0, Problems: discoveryProblems(report.Errors)}
	present := make(map[string]bool)
	problem := func(id, format string, args ...any) {
		result.Kept[id] = true
		result.Problems = append(result.Problems, fmt.Sprintf(format, args...))
	}
	for _, entry := range report.Projects {
		workspace := filepath.Join(workspaces, entry.ProjectID)
		if !domain.SafeID(entry.ProjectID) || entry.Root != filepath.Join(workspace, "src") {
			continue
		}
		present[entry.ProjectID] = true
		if !slices.ContainsFunc(entry.Roots, func(root struct{ Path, Kind string }) bool { return root.Path == entry.Root }) {
			problem(entry.ProjectID, "project %q has no canonical checkout directory", entry.ProjectID)
			continue
		}
		result.Checkouts[entry.ProjectID] = checkout{Name: entry.Name, Folder: entry.Root}
		for _, root := range entry.Roots {
			if root.Path == entry.Root {
				continue
			}
			path, err := filepath.Rel(workspace, root.Path)
			if root.Kind == "unknown" && result.Partial {
				// Discovery reported the unverifiable root; it is neither added nor retired.
				result.Kept[entry.ProjectID] = true
				continue
			}
			if err != nil || root.Kind != "git" || !validCheckoutPath(path) || filepath.Join(workspace, path) != root.Path {
				problem(entry.ProjectID, "checkout discovery returned an invalid root for %q", entry.ProjectID)
				continue
			}
			result.Checkouts[checkoutKey(entry.ProjectID, path)] = checkout{
				Name: entry.Name + "/" + strings.TrimPrefix(path, "src/"), Folder: root.Path,
			}
		}
	}
	if !result.Partial {
		result.proveAbsence(workspaces, known, report.Absent, present, problem)
	}
	slices.Sort(result.Problems)
	result.Problems = slices.Compact(result.Problems)
	return result, nil
}

func (result inventory) proveAbsence(workspaces string, known, folders []string, present map[string]bool,
	problem func(id, format string, args ...any),
) {
	absent := make(map[string]bool)
	for _, folder := range folders {
		absent[folder] = true
	}
	for _, key := range known {
		if absent[knownFolder(workspaces, key)] {
			result.Gone[key] = true
			continue
		}
		if id, path := splitKey(key); path == "" && !present[id] {
			if info, err := os.Lstat(filepath.Join(workspaces, id)); err == nil && !info.IsDir() {
				problem(id, "workspace %q is no longer a directory", id)
			} else {
				problem(id, "invalid project metadata for %q", id)
			}
		}
	}
}
