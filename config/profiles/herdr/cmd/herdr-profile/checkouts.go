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
var discoveryProgram = []string{"python3", "-B", "-E", "-s", "/usr/local/libexec/subyard/herdr-discovery/checkouts.py"}

// checkout is the desired space of one project checkout.
type checkout struct {
	Name, Folder string
}

// inventory holds the desired checkouts of the present projects and the known
// checkouts whose absence discovery proved.
type inventory struct {
	Checkouts map[string]checkout
	Gone      map[string]bool
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

func validKey(key string) bool {
	id, path := splitKey(key)
	return domain.SafeID(id) && (path == "" && !strings.Contains(key, "/") || validCheckoutPath(path))
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
	stdout := &limitedBuffer{limit: discoveryReportLimit}
	command.Stdout = stdout
	if err := command.Run(); err != nil {
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
	refusals := make([]error, 0, len(messages))
	for _, message := range messages {
		refusals = append(refusals, errors.New("checkout discovery: "+strings.ToValidUTF8(message, "?")))
	}
	return domain.CollectRefusals(refusals...)
}

func knownFolder(workspaces, key string) string {
	id, path := splitKey(key)
	return filepath.Join(workspaces, id, path)
}

// scanCheckouts reports src, every Git root below it and the Git roots beside it
// for each project, and which known checkouts are proven absent.
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
	// A partial scan is no evidence for retiring anything.
	if err := discoveryErrors(report.Errors); err != nil {
		return inventory{}, err
	}
	result := inventory{Checkouts: make(map[string]checkout), Gone: make(map[string]bool)}
	present := make(map[string]bool)
	var refusals []error
	for _, entry := range report.Projects {
		workspace := filepath.Join(workspaces, entry.ProjectID)
		if !domain.SafeID(entry.ProjectID) || entry.Root != filepath.Join(workspace, "src") {
			continue
		}
		present[entry.ProjectID] = true
		if !slices.ContainsFunc(entry.Roots, func(root struct{ Path, Kind string }) bool { return root.Path == entry.Root }) {
			refusals = append(refusals, fmt.Errorf("project %q has no canonical checkout directory", entry.ProjectID))
			continue
		}
		result.Checkouts[entry.ProjectID] = checkout{Name: entry.Name, Folder: entry.Root}
		for _, root := range entry.Roots {
			if root.Path == entry.Root {
				continue
			}
			path, err := filepath.Rel(workspace, root.Path)
			if err != nil || root.Kind != "git" || !validCheckoutPath(path) || filepath.Join(workspace, path) != root.Path {
				refusals = append(refusals, fmt.Errorf("checkout discovery returned an invalid root for %q", entry.ProjectID))
				continue
			}
			result.Checkouts[checkoutKey(entry.ProjectID, path)] = checkout{
				Name: entry.Name + "/" + strings.TrimPrefix(path, "src/"), Folder: root.Path,
			}
		}
	}
	absent := make(map[string]bool)
	for _, folder := range report.Absent {
		absent[folder] = true
	}
	for _, key := range known {
		if absent[knownFolder(workspaces, key)] {
			result.Gone[key] = true
			continue
		}
		if id, path := splitKey(key); path == "" && !present[id] {
			if info, err := os.Lstat(filepath.Join(workspaces, id)); err == nil && !info.IsDir() {
				refusals = append(refusals, fmt.Errorf("workspace %q is no longer a directory", id))
			} else {
				refusals = append(refusals, fmt.Errorf("invalid project metadata for %q", id))
			}
		}
	}
	return result, domain.CollectRefusals(refusals...)
}
