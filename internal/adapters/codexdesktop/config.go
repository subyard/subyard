// Package codexdesktop owns the desktop client's declarative SSH project import.
package codexdesktop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Subyard/Subyard/internal/clientprojects"
)

const ImportURL = "codex://codex-app/apply-config"
const maxConfigBytes = 8 << 20

type declaration struct {
	Version        int          `json:"version"`
	Connections    []connection `json:"remoteConnections"`
	MaxRetries     *uint64      `json:"remoteConnectionMaxRetryAttempts,omitempty"`
	ConnectTimeout *uint64      `json:"sshConnectTimeoutSeconds,omitempty"`
}

type connection struct {
	Alias    string    `json:"sshAlias"`
	Projects []project `json:"projects"`
}

type project struct {
	Path  string `json:"remotePath"`
	Label string `json:"label,omitempty"`
}

// ConfigPath resolves the GUI home explicitly; it never reads ambient home state.
func ConfigPath(operatorHome, codexHome, override string) (string, error) {
	if override != "" {
		if !filepath.IsAbs(override) || strings.ContainsRune(override, 0) {
			return "", errors.New("desktop config override must be an absolute path")
		}
		return filepath.Clean(override), nil
	}
	if codexHome == "" {
		if !filepath.IsAbs(operatorHome) {
			return "", errors.New("operator home must be an absolute path")
		}
		codexHome = filepath.Join(operatorHome, ".codex")
	}
	if !filepath.IsAbs(codexHome) || strings.ContainsRune(codexHome, 0) {
		return "", errors.New("CODEX_HOME must be an absolute path")
	}
	return filepath.Join(codexHome, "codex-app", "config.json"), nil
}

// OpenURL requests import through the Linux desktop handler. Success confirms
// only that the launch command succeeded, not that the GUI imported projects.
func OpenURL(ctx context.Context, url string) error {
	if url != ImportURL {
		return errors.New("unsupported desktop import URL")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "xdg-open", url)
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("desktop URL handler: %w", err)
	}
	return nil
}

// Prepare validates and assesses without writes, locking, or launching a GUI.
func Prepare(target string, input clientprojects.Export, opener func(context.Context, string) error) (clientprojects.Plan, error) {
	return prepare(target, input, opener, nil)
}

func prepare(target string, input clientprojects.Export, opener func(context.Context, string) error, fault func(string) error) (clientprojects.Plan, error) {
	plan := clientprojects.Plan{Target: filepath.Clean(target)}
	if !filepath.IsAbs(target) || strings.ContainsRune(target, 0) || filepath.Base(plan.Target) == "/" {
		return plan, errors.New("desktop config path must name an absolute file")
	}
	baseline, ancestors, err := inspect(plan.Target)
	if err != nil {
		return plan, fmt.Errorf("inspect desktop config: %w", err)
	}
	exportStatus := "desktop export is saved at " + plan.Target
	if !baseline.Exists && len(input.Projects) == 0 {
		exportStatus = "selected yard has no registered projects; no desktop declaration was written"
	}
	plan.Open = func(ctx context.Context) error {
		if opener == nil {
			return fmt.Errorf("%s; open Codex and import its config, or retry with a desktop URL handler", exportStatus)
		}
		if err := opener(ctx, ImportURL); err != nil {
			return fmt.Errorf("%s; import request failed: %w; open Codex or retry the open action after fixing the desktop URL handler", exportStatus, err)
		}
		return nil
	}
	doc := declaration{Version: 1, Connections: []connection{}}
	if baseline.Exists {
		if err := decode(baseline.Content, &doc); err != nil {
			return plan, fmt.Errorf("invalid desktop config: %w", err)
		}
	}
	if err := validateDeclaration(doc); err != nil {
		return plan, err
	}
	plan.Apply = func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, currentAncestors, err := inspect(plan.Target)
		if err != nil {
			return err
		}
		if !sameSnapshot(current, baseline) || len(currentAncestors) != len(ancestors) {
			return errStale
		}
		for i := range ancestors {
			if ancestors[i] != currentAncestors[i] {
				return errStale
			}
		}
		return nil
	}
	if len(input.Projects) == 0 {
		return plan, nil
	}
	if err := validateText(input.HostID, "owner identity"); err != nil {
		return plan, err
	}
	if err := validateText(input.Yard, "yard identity"); err != nil {
		return plan, err
	}
	if err := validateAlias(input.SSHHost); err != nil {
		return plan, err
	}
	selected := connection{Alias: input.SSHHost, Projects: []project{}}
	for _, p := range input.Projects {
		if err := validateText(p.Name, "project name"); err != nil {
			return plan, err
		}
		selected.Projects = append(selected.Projects, project{Path: p.Path, Label: p.Name + " / " + input.HostID + "/" + input.Yard})
	}
	if err := validateConnection(selected); err != nil {
		return plan, err
	}
	sort.Slice(selected.Projects, func(i, j int) bool { return selected.Projects[i].Path < selected.Projects[j].Path })
	index := -1
	for i, c := range doc.Connections {
		if strings.EqualFold(c.Alias, selected.Alias) && c.Alias != selected.Alias {
			return plan, errors.New("selected SSH alias has a case-only collision with an existing connection")
		}
		if c.Alias == selected.Alias {
			index = i
			break
		}
	}
	if index < 0 {
		doc.Connections = append(doc.Connections, selected)
		plan.Added = len(selected.Projects)
		plan.Changed = true
	} else {
		c := &doc.Connections[index]
		byPath := make(map[string]int, len(c.Projects))
		for i, old := range c.Projects {
			byPath[fold(path.Clean(old.Path))] = i
		}
		for _, p := range selected.Projects {
			cleanPath := path.Clean(p.Path)
			key := fold(cleanPath)
			if i, found := byPath[key]; found {
				if path.Clean(c.Projects[i].Path) != cleanPath {
					return plan, fmt.Errorf("case-only remote path collision for SSH alias %q", c.Alias)
				}
				if c.Projects[i].Label == "" {
					c.Projects[i].Label = p.Label
					plan.Changed = true
				}
			} else {
				byPath[key] = len(c.Projects)
				c.Projects = append(c.Projects, p)
				plan.Added++
				plan.Changed = true
			}
		}
	}
	if !plan.Changed {
		return plan, nil
	}
	if err := validateDeclaration(doc); err != nil {
		return plan, err
	}
	desired, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return plan, err
	}
	desired = append(desired, '\n')
	if len(desired) > maxConfigBytes {
		return plan, errors.New("desktop config exceeds its size bound")
	}
	backup, _, err := inspect(plan.Target + ".subyard-backup")
	if err != nil {
		return plan, fmt.Errorf("inspect desktop config backup: %w", err)
	}
	plan.Apply = func(ctx context.Context) error {
		return apply(ctx, plan.Target, baseline, backup, ancestors, desired, fault)
	}
	return plan, nil
}

func validateText(value, role string) error {
	if !utf8.ValidString(value) || strings.TrimSpace(value) == "" || len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("invalid %s", role)
	}
	return nil
}

func validateAlias(alias string) error {
	if !utf8.ValidString(alias) || alias == "" || len(alias) > 255 || strings.HasPrefix(alias, "-") || strings.ContainsAny(alias, " /") || strings.ContainsFunc(alias, unicode.IsControl) {
		return errors.New("invalid SSH alias")
	}
	return nil
}

func validateDeclaration(doc declaration) error {
	if doc.Version != 1 || doc.Connections == nil {
		return errors.New("desktop config requires version 1 and remoteConnections array")
	}
	seen := map[string]bool{}
	for _, c := range doc.Connections {
		alias := fold(c.Alias)
		if seen[alias] {
			return errors.New("duplicate or case-only SSH alias collision")
		}
		seen[alias] = true
		if err := validateConnection(c); err != nil {
			return err
		}
	}
	return nil
}

func validateConnection(c connection) error {
	if err := validateAlias(c.Alias); err != nil {
		return err
	}
	if c.Projects == nil {
		return errors.New("desktop connection requires projects array")
	}
	seen := map[string]bool{}
	for _, p := range c.Projects {
		if !utf8.ValidString(p.Path) || !path.IsAbs(p.Path) || len(p.Path) > 4096 || strings.ContainsFunc(p.Path, unicode.IsControl) {
			return errors.New("invalid absolute remote project path")
		}
		key := fold(path.Clean(p.Path))
		if seen[key] {
			return fmt.Errorf("duplicate or case-only remote path collision for SSH alias %q", c.Alias)
		}
		seen[key] = true
		if len(p.Label) > 4096 || strings.ContainsFunc(p.Label, unicode.IsControl) {
			return errors.New("invalid project label")
		}
	}
	return nil
}

func decode(content []byte, doc *declaration) error {
	// Token validation catches duplicate members (including escaped keys), null,
	// and missing required properties before decoding into the native schema.
	if !utf8.Valid(content) {
		return errors.New("desktop config must be valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	if err := checkJSONValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("desktop config has trailing JSON")
	}
	decoder = json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	return decoder.Decode(doc)
}

func checkJSONValue(d *json.Decoder, depth int) error {
	if depth > 12 {
		return errors.New("desktop config exceeds JSON nesting bound")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	if token == nil {
		return errors.New("desktop config must not contain null")
	}
	delim, container := token.(json.Delim)
	if !container {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			keyToken, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok || seen[key] {
				return errors.New("duplicate JSON member")
			}
			// encoding/json accepts case-insensitive struct field names. Check
			// exact schema keys here so a second spelling cannot override one.
			allowed := false
			switch depth {
			case 0:
				allowed = key == "version" || key == "remoteConnections" || key == "remoteConnectionMaxRetryAttempts" || key == "sshConnectTimeoutSeconds"
			case 2:
				allowed = key == "sshAlias" || key == "projects"
			case 4:
				allowed = key == "remotePath" || key == "label"
			}
			if !allowed {
				return fmt.Errorf("unknown desktop config member %q", key)
			}
			seen[key] = true
			if err := checkJSONValue(d, depth+1); err != nil {
				return err
			}
		}
		if depth == 0 && (!seen["version"] || !seen["remoteConnections"]) {
			return errors.New("missing desktop config fields")
		}
		if depth == 2 && (!seen["sshAlias"] || !seen["projects"]) {
			return errors.New("missing desktop connection fields")
		}
		if depth == 4 && !seen["remotePath"] {
			return errors.New("missing desktop project path")
		}
	case '[':
		for d.More() {
			if err := checkJSONValue(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid JSON container")
	}
	_, err = d.Token()
	return err
}

func fold(value string) string {
	return strings.Map(func(r rune) rune {
		minimum := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < minimum {
				minimum = next
			}
		}
		return minimum
	}, value)
}
