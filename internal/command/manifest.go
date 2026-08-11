package command

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

const fieldCount = 14

type RemotePlane string

const (
	RemoteLocal   RemotePlane = "local"
	RemoteForward RemotePlane = "forward"
	RemoteDeny    RemotePlane = "deny"
)

type Effect string

const (
	EffectRead   Effect = "read"
	EffectMutate Effect = "mutate"
)

type Confirmation string

const (
	ConfirmationNever   Confirmation = "never"
	ConfirmationDynamic Confirmation = "dynamic"
)

type Visibility string

const (
	VisibilityPublic Visibility = "public"
	VisibilityHidden Visibility = "hidden"
)

type Definition struct {
	Name         string
	Aliases      []string
	Handler      string
	Arg0         string
	Remote       RemotePlane
	Effect       Effect
	Confirmation Confirmation
	Visibility   Visibility
	Section      string
	Completion   string
	Display      string
	Summary      string
	Options      []string
	Verbs        []string
}

type Manifest struct {
	commands []Definition
	lookup   map[string]int
}

func Parse(reader io.Reader) (Manifest, error) {
	manifest := Manifest{lookup: make(map[string]int)}
	scanner := bufio.NewScanner(reader)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) != fieldCount {
			return Manifest{}, fmt.Errorf("command manifest line %d has %d fields, expected %d", lineNumber, len(fields), fieldCount)
		}
		definition := Definition{
			Name:         fields[0],
			Aliases:      splitNonEmpty(fields[1], ","),
			Handler:      fields[2],
			Arg0:         fields[3],
			Remote:       RemotePlane(fields[4]),
			Effect:       Effect(fields[5]),
			Confirmation: Confirmation(fields[6]),
			Visibility:   Visibility(fields[7]),
			Section:      fields[8],
			Completion:   fields[9],
			Display:      fields[10],
			Summary:      fields[11],
			Options:      strings.Fields(fields[12]),
			Verbs:        strings.Fields(fields[13]),
		}
		if err := definition.Validate(); err != nil {
			return Manifest{}, fmt.Errorf("command manifest line %d: %w", lineNumber, err)
		}
		index := len(manifest.commands)
		for _, name := range append([]string{definition.Name}, definition.Aliases...) {
			if _, exists := manifest.lookup[name]; exists {
				return Manifest{}, fmt.Errorf("command manifest line %d: duplicate command or alias %q", lineNumber, name)
			}
			manifest.lookup[name] = index
		}
		manifest.commands = append(manifest.commands, definition)
	}
	if err := scanner.Err(); err != nil {
		return Manifest{}, fmt.Errorf("read command manifest: %w", err)
	}
	if len(manifest.commands) == 0 {
		return Manifest{}, errors.New("command manifest is empty")
	}
	return manifest, nil
}

func (definition Definition) Validate() error {
	if !safeToken(definition.Name) {
		return fmt.Errorf("invalid command name %q", definition.Name)
	}
	for _, alias := range definition.Aliases {
		if !safeToken(alias) {
			return fmt.Errorf("invalid alias %q", alias)
		}
	}
	if !validHandler(definition.Handler) {
		return fmt.Errorf("invalid handler %q", definition.Handler)
	}
	if definition.Arg0 != "" && !safeToken(definition.Arg0) {
		return fmt.Errorf("invalid handler argument %q", definition.Arg0)
	}
	if !slices.Contains([]RemotePlane{RemoteLocal, RemoteForward, RemoteDeny}, definition.Remote) {
		return fmt.Errorf("invalid remote plane %q", definition.Remote)
	}
	if definition.Effect != EffectRead && definition.Effect != EffectMutate {
		return fmt.Errorf("invalid effect %q", definition.Effect)
	}
	if !slices.Contains([]Confirmation{ConfirmationNever, ConfirmationDynamic}, definition.Confirmation) {
		return fmt.Errorf("invalid confirmation policy %q", definition.Confirmation)
	}
	if definition.Visibility != VisibilityPublic && definition.Visibility != VisibilityHidden {
		return fmt.Errorf("invalid visibility %q", definition.Visibility)
	}
	if !safeToken(definition.Section) || !safeToken(definition.Completion) {
		return errors.New("section and completion provider must be safe tokens")
	}
	if definition.Display == "" || definition.Summary == "" {
		return errors.New("display and summary are required")
	}
	for _, option := range definition.Options {
		if !strings.HasPrefix(option, "-") || strings.ContainsFunc(option, unicode.IsSpace) {
			return fmt.Errorf("invalid option %q", option)
		}
	}
	for _, verb := range definition.Verbs {
		if !safeToken(verb) {
			return fmt.Errorf("invalid verb %q", verb)
		}
	}
	return nil
}

func (manifest Manifest) Lookup(name string) (Definition, bool) {
	index, ok := manifest.lookup[name]
	if !ok {
		return Definition{}, false
	}
	return manifest.commands[index], true
}

func (manifest Manifest) Commands() []Definition {
	return slices.Clone(manifest.commands)
}

func (manifest Manifest) PublicNames() []string {
	names := make([]string, 0, len(manifest.commands))
	for _, definition := range manifest.commands {
		if definition.Visibility == VisibilityPublic {
			names = append(names, definition.Name)
		}
	}
	return names
}

func (manifest Manifest) Section(name string) []Definition {
	definitions := make([]Definition, 0)
	for _, definition := range manifest.commands {
		if definition.Visibility == VisibilityPublic && definition.Section == name {
			definitions = append(definitions, definition)
		}
	}
	return definitions
}

func safeToken(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') &&
			!(char >= '0' && char <= '9') && char != '_' && char != '-' {
			return false
		}
	}
	return true
}

func validHandler(handler string) bool {
	if slices.Contains([]string{
		"@authorize", "@help", "@info", "@init", "@keys", "@lifecycle", "@list", "@logs", "@migrate", "@provision", "@teardown", "@test-vms", "@update",
		"@project", "@project-state", "@remote", "@resource", "@rpc", "@shell", "@space", "@state", "@status", "@usage", "@yards",
	}, handler) {
		return true
	}
	if handler == "" || filepath.IsAbs(handler) || strings.HasPrefix(handler, "-") || strings.Contains(handler, "..") {
		return false
	}
	return filepath.Clean(handler) == handler && !strings.ContainsAny(handler, " \\|;&`$(){}[]<>")
}

func (definition Definition) Row() string {
	return strings.Join([]string{
		definition.Name,
		strings.Join(definition.Aliases, ","),
		definition.Handler,
		definition.Arg0,
		string(definition.Remote),
		string(definition.Effect),
		string(definition.Confirmation),
		string(definition.Visibility),
		definition.Section,
		definition.Completion,
		definition.Display,
		definition.Summary,
		strings.Join(definition.Options, " "),
		strings.Join(definition.Verbs, " "),
	}, "|")
}

func splitNonEmpty(value, separator string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, separator)
}
