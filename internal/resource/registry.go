package resource

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Subyard/Subyard/internal/domain"
)

type Definition struct {
	Profile  string
	Name     string
	Command  string
	Handler  string
	BringUp  string
	Shutdown string
	Verbs    []string
	Title    string
	path     string
	actions  []actionDeclaration
}

func (definition Definition) HandlerPath() string { return definition.path }

type Registry struct {
	definitions       []Definition
	actionDefinitions []domain.ActionDefinition
	byCommand         map[string]int
	byName            map[string]int
}

type actionDeclaration struct {
	localID string
	verb    string
	action  domain.ActionID
}

type descriptorValues struct {
	singletons map[string]string
	actions    []string
}

func Load(root string) (Registry, error) {
	pattern := filepath.Join(root, "config", "profiles", "*", "resources", "*.res")
	files, err := filepath.Glob(pattern)
	if err != nil {
		return Registry{}, err
	}
	registry := Registry{byCommand: make(map[string]int), byName: make(map[string]int)}
	qualifiedActions := make(map[domain.ActionID]struct{})
	for _, file := range files {
		definition, actionDefinitions, err := loadDefinition(root, file)
		if err != nil {
			return Registry{}, err
		}
		if _, duplicate := registry.byCommand[definition.Command]; duplicate {
			return Registry{}, fmt.Errorf("duplicate resource command %q", definition.Command)
		}
		if _, duplicate := registry.byName[definition.Name]; duplicate {
			return Registry{}, fmt.Errorf("duplicate resource name %q", definition.Name)
		}
		for _, action := range actionDefinitions {
			if _, duplicate := qualifiedActions[action.Action]; duplicate {
				return Registry{}, fmt.Errorf("duplicate qualified resource action %q", action.Action)
			}
			qualifiedActions[action.Action] = struct{}{}
		}
		index := len(registry.definitions)
		registry.byCommand[definition.Command] = index
		registry.byName[definition.Name] = index
		registry.definitions = append(registry.definitions, definition)
		registry.actionDefinitions = append(registry.actionDefinitions, actionDefinitions...)
	}
	return registry, nil
}

func (registry Registry) Definitions() []Definition {
	definitions := make([]Definition, len(registry.definitions))
	for index, definition := range registry.definitions {
		definitions[index] = cloneDefinition(definition)
	}
	return definitions
}

func (registry Registry) ActionDefinitions() []domain.ActionDefinition {
	definitions := make([]domain.ActionDefinition, len(registry.actionDefinitions))
	for index, definition := range registry.actionDefinitions {
		definition.Impacts = slices.Clone(definition.Impacts)
		definitions[index] = definition
	}
	return definitions
}

func (registry Registry) Lookup(value string) (Definition, bool) {
	if index, ok := registry.byCommand[value]; ok {
		return cloneDefinition(registry.definitions[index]), true
	}
	if index, ok := registry.byName[value]; ok {
		return cloneDefinition(registry.definitions[index]), true
	}
	return Definition{}, false
}

func (registry Registry) LookupAction(resource, verb, localID string) (domain.ActionID, bool) {
	definition, ok := registry.definition(resource)
	if !ok {
		return "", false
	}
	for _, action := range definition.actions {
		if action.verb == verb && action.localID == localID {
			return action.action, true
		}
	}
	return "", false
}

func (registry Registry) definition(value string) (Definition, bool) {
	if index, ok := registry.byCommand[value]; ok {
		return registry.definitions[index], true
	}
	if index, ok := registry.byName[value]; ok {
		return registry.definitions[index], true
	}
	return Definition{}, false
}

func cloneDefinition(definition Definition) Definition {
	definition.Verbs = slices.Clone(definition.Verbs)
	definition.actions = slices.Clone(definition.actions)
	return definition
}

func loadDefinition(root, path string) (Definition, []domain.ActionDefinition, error) {
	profile := filepath.Base(filepath.Dir(filepath.Dir(path)))
	name := strings.TrimSuffix(filepath.Base(path), ".res")
	if !domain.SafeName(profile) || !domain.SafeName(name) {
		return Definition{}, nil, fmt.Errorf("invalid resource identity in %s", path)
	}
	values, err := readDescriptor(path)
	if err != nil {
		return Definition{}, nil, err
	}
	command := defaultValue(values.singletons["COMMAND"], name)
	bringUp := defaultValue(values.singletons["BRINGUP"], "up")
	shutdown := defaultValue(values.singletons["SHUTDOWN"], "down")
	handler := values.singletons["HANDLER"]
	title := values.singletons["TITLE"]
	if !domain.SafeName(command) || !domain.SafeName(bringUp) || !domain.SafeName(shutdown) ||
		handler == "" || title == "" || len(values.actions) == 0 {
		return Definition{}, nil, fmt.Errorf("resource descriptor is incomplete: %s", path)
	}
	declarations := make([]actionDeclaration, 0, len(values.actions))
	actionDefinitions := make([]domain.ActionDefinition, 0, len(values.actions))
	verbs := make([]string, 0, len(values.actions))
	localIDs := make(map[string]struct{}, len(values.actions))
	for _, record := range values.actions {
		fields := strings.Fields(record)
		if len(fields) != 4 {
			return Definition{}, nil, fmt.Errorf("invalid ACTION record %q in %s", record, path)
		}
		localID, verb, class, recoveryName := fields[0], fields[1], fields[2], fields[3]
		if !domain.SafeName(localID) || !domain.SafeName(verb) {
			return Definition{}, nil, fmt.Errorf("invalid resource action %q in %s", record, path)
		}
		if _, duplicate := localIDs[localID]; duplicate {
			return Definition{}, nil, fmt.Errorf("duplicate resource action local ID %q in %s", localID, path)
		}
		localIDs[localID] = struct{}{}
		effect, impacts, ok := assessmentClass(class)
		if !ok {
			return Definition{}, nil, fmt.Errorf("unknown resource assessment class %q in %s", class, path)
		}
		recovery, ok := recoveryClass(recoveryName)
		if !ok {
			return Definition{}, nil, fmt.Errorf("unknown resource recovery class %q in %s", recoveryName, path)
		}
		qualified := domain.ActionID("resource." + profile + "." + name + "." + localID)
		actionDefinition := domain.ActionDefinition{
			Action: qualified, Summary: title + ": " + localID, Effect: effect,
			Impacts: slices.Clone(impacts), Recovery: recovery,
		}
		if _, err := domain.NewActionRegistry([]domain.ActionDefinition{actionDefinition}); err != nil {
			return Definition{}, nil, fmt.Errorf("invalid resource action %q in %s: %w", localID, path, err)
		}
		declarations = append(declarations, actionDeclaration{localID: localID, verb: verb, action: qualified})
		actionDefinitions = append(actionDefinitions, actionDefinition)
		if !slices.Contains(verbs, verb) {
			verbs = append(verbs, verb)
		}
	}
	if !slices.Contains(verbs, bringUp) || !slices.Contains(verbs, shutdown) {
		return Definition{}, nil, fmt.Errorf("resource lifecycle verbs are missing in %s", path)
	}
	profileRoot := filepath.Join(root, "config", "profiles", profile)
	handlerPath := filepath.Clean(filepath.Join(profileRoot, handler))
	relative, err := filepath.Rel(profileRoot, handlerPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return Definition{}, nil, fmt.Errorf("resource handler escapes profile: %s", path)
	}
	info, err := os.Lstat(handlerPath)
	if err != nil {
		return Definition{}, nil, fmt.Errorf("inspect resource handler %s: %w", handlerPath, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o111 == 0 {
		return Definition{}, nil, fmt.Errorf("resource handler is not an executable regular file: %s", handlerPath)
	}
	resolvedProfileRoot, err := filepath.EvalSymlinks(profileRoot)
	if err != nil {
		return Definition{}, nil, fmt.Errorf("resolve resource profile %s: %w", profileRoot, err)
	}
	resolvedHandlerPath, err := filepath.EvalSymlinks(handlerPath)
	if err != nil {
		return Definition{}, nil, fmt.Errorf("resolve resource handler %s: %w", handlerPath, err)
	}
	resolvedRelative, err := filepath.Rel(resolvedProfileRoot, resolvedHandlerPath)
	if err != nil || resolvedRelative == ".." || strings.HasPrefix(resolvedRelative, ".."+string(filepath.Separator)) {
		return Definition{}, nil, fmt.Errorf("resource handler escapes resolved profile: %s", path)
	}
	return Definition{
		Profile: profile, Name: name, Command: command, Handler: handler,
		BringUp: bringUp, Shutdown: shutdown, Verbs: verbs, Title: title, path: resolvedHandlerPath,
		actions: declarations,
	}, actionDefinitions, nil
}

func assessmentClass(value string) (domain.ActionEffect, []domain.ActionImpact, bool) {
	switch value {
	case "read-only":
		return domain.ActionRead, nil, true
	case "session":
		return domain.ActionSession, nil, true
	case "bounded-write":
		return domain.ActionBoundedWrite, nil, true
	case "yard-change":
		return domain.ActionMutation, []domain.ActionImpact{domain.ImpactYardRuntime}, true
	case "host-change":
		return domain.ActionMutation, []domain.ActionImpact{domain.ImpactHostOS}, true
	case "external-change":
		return domain.ActionMutation, []domain.ActionImpact{domain.ImpactExternalSystem}, true
	case "security-change":
		return domain.ActionMutation, []domain.ActionImpact{domain.ImpactAccess, domain.ImpactSecurity, domain.ImpactTrust}, true
	case "shared-workload-change":
		return domain.ActionMutation, []domain.ActionImpact{domain.ImpactSharedWorkload}, true
	case "runtime-destruction":
		return domain.ActionDestruction, []domain.ActionImpact{domain.ImpactYardRuntime}, true
	case "persistent-data-destruction":
		return domain.ActionDestruction, []domain.ActionImpact{domain.ImpactPersistentData}, true
	default:
		return "", nil, false
	}
}

func recoveryClass(value string) (domain.RecoveryClass, bool) {
	switch domain.RecoveryClass(value) {
	case domain.RecoveryNotNeeded, domain.RecoveryReversible, domain.RecoveryRecreatable, domain.RecoveryIrreversible:
		return domain.RecoveryClass(value), true
	default:
		return "", false
	}
}

func readDescriptor(path string) (descriptorValues, error) {
	file, err := os.Open(path)
	if err != nil {
		return descriptorValues{}, err
	}
	defer file.Close()
	allowed := map[string]struct{}{
		"COMMAND": {}, "HANDLER": {}, "TITLE": {}, "ACTION": {}, "BRINGUP": {}, "SHUTDOWN": {},
	}
	values := descriptorValues{singletons: make(map[string]string)}
	scanner := bufio.NewScanner(file)
	line := 0
	for scanner.Scan() {
		line++
		record := strings.TrimSpace(scanner.Text())
		if record == "" || strings.HasPrefix(record, "#") {
			continue
		}
		name, value, ok := strings.Cut(record, "=")
		if !ok {
			return descriptorValues{}, fmt.Errorf("%s:%d: descriptor assignments only", path, line)
		}
		if _, ok := allowed[name]; !ok {
			return descriptorValues{}, fmt.Errorf("%s:%d: unknown descriptor field %q", path, line, name)
		}
		if _, duplicate := values.singletons[name]; duplicate && name != "ACTION" {
			return descriptorValues{}, fmt.Errorf("%s:%d: duplicate descriptor field %q", path, line, name)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') ||
			(value[0] == '\'' && value[len(value)-1] == '\'')) {
			value = value[1 : len(value)-1]
		}
		if strings.Contains(value, "$(") || strings.ContainsRune(value, '`') || strings.ContainsRune(value, 0) {
			return descriptorValues{}, fmt.Errorf("%s:%d: unsafe descriptor value", path, line)
		}
		if name == "ACTION" {
			values.actions = append(values.actions, value)
		} else {
			values.singletons[name] = value
		}
	}
	if err := scanner.Err(); err != nil {
		return descriptorValues{}, err
	}
	return values, nil
}

func defaultValue(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
