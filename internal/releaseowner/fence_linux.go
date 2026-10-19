package releaseowner

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/Subyard/Subyard/internal/ports"
)

type FenceVectorRecipe struct {
	Version    int                              `json:"version"`
	Expected   []ports.NativeResourceGeneration `json:"expected"`
	Generation string                           `json:"generation"`
}

func ParseFenceVectorRecipe(raw string) (FenceVectorRecipe, error) {
	var value FenceVectorRecipe
	if strict([]byte(raw), &value) != nil || value.Version != 2 || len(value.Expected) < 2 || len(value.Expected) > 3 || !fingerprint(value.Generation) {
		return value, errors.New("native vector fence recipe")
	}
	roles := []string{"engine", "service", "timer"}
	for index, entry := range value.Expected {
		if entry.Role != roles[index] || (entry.Value != "0" && !fingerprint(entry.Value)) {
			return value, errors.New("native vector fence roles")
		}
	}
	canonical, err := json.Marshal(value)
	if err != nil || !bytes.Equal(canonical, []byte(raw)) {
		return value, errors.New("noncanonical native vector fence recipe")
	}
	return value, nil
}

func PrepareFenceVectorRecipe(session string, target Target, expected []ports.NativeResourceGeneration) (string, error) {
	raw, err := json.Marshal(struct {
		Session, Owner, Yard, Project, InstanceUUID, Desired, Source string
		Expected                                                     []ports.NativeResourceGeneration
	}{session, target.Owner, target.Yard, target.Project, target.InstanceUUID, target.Desired, target.Source, expected})
	if err != nil || !fingerprint(session) {
		return "", errors.New("native vector fence preparation")
	}
	value := FenceVectorRecipe{2, expected, digest(raw)}
	raw, err = json.Marshal(value)
	if err != nil {
		return "", err
	}
	_, err = ParseFenceVectorRecipe(string(raw))
	return string(raw), err
}

// FenceRecipe is a resource-local CAS recipe, frozen in the consented target.
// The owner session supplies the generation nonce; the component does not
// choose recovery, replace an invocation or treat this as desired runtime state.
type FenceRecipe struct {
	Version    int    `json:"version"`
	Expected   string `json:"expected"`
	Generation string `json:"generation"`
}

func ParseFenceRecipe(raw string) (FenceRecipe, error) {
	var value FenceRecipe
	if strict([]byte(raw), &value) != nil || value.Version != 1 || (value.Expected != "0" && !fingerprint(value.Expected)) || !fingerprint(value.Generation) {
		return value, errors.New("native fence recipe")
	}
	canonical, err := json.Marshal(value)
	if err != nil || !bytes.Equal(canonical, []byte(raw)) {
		return value, errors.New("noncanonical native fence recipe")
	}
	return value, nil
}

func PrepareFenceRecipe(session string, target Target, expected string) (string, error) {
	if !fingerprint(session) || (expected != "0" && !fingerprint(expected)) {
		return "", errors.New("native fence preparation")
	}
	raw, err := json.Marshal(struct {
		Session      string `json:"session"`
		Owner        string `json:"owner"`
		Yard         string `json:"yard"`
		Project      string `json:"project"`
		InstanceUUID string `json:"instanceUuid"`
		Desired      string `json:"desired"`
		Source       string `json:"source"`
		Expected     string `json:"expected"`
	}{session, target.Owner, target.Yard, target.Project, target.InstanceUUID, target.Desired, target.Source, expected})
	if err != nil {
		return "", err
	}
	value := FenceRecipe{Version: 1, Expected: expected, Generation: digest(raw)}
	raw, err = json.Marshal(value)
	return string(raw), err
}

func fenceObserved(target Target, successor string) (string, error) {
	if before, err := ParseFenceVectorRecipe(target.Before); err == nil {
		if !fingerprint(successor) || successor == before.Generation {
			return "", errors.New("native vector fence successor")
		}
		current := make([]ports.NativeResourceGeneration, len(before.Expected))
		for index, entry := range before.Expected {
			if entry.Value == successor {
				return "", errors.New("native vector fence successor")
			}
			current[index] = ports.NativeResourceGeneration{Role: entry.Role, Value: successor}
		}
		raw, err := json.Marshal(struct {
			Version    int                              `json:"version"`
			Expected   []ports.NativeResourceGeneration `json:"expected"`
			Generation string                           `json:"generation"`
			Successor  string                           `json:"successor"`
			Current    []ports.NativeResourceGeneration `json:"current"`
			Source     string                           `json:"source"`
		}{2, before.Expected, before.Generation, successor, current, target.Source})
		return string(raw), err
	}
	before, err := ParseFenceRecipe(target.Before)
	if err != nil || !fingerprint(successor) || successor == before.Expected || successor == before.Generation {
		return "", errors.New("native fence successor")
	}
	raw, err := json.Marshal(struct {
		Version    int    `json:"version"`
		Expected   string `json:"expected"`
		Generation string `json:"generation"`
		Successor  string `json:"successor"`
		Current    string `json:"current"`
		Source     string `json:"source"`
	}{1, before.Expected, before.Generation, successor, successor, target.Source})
	return string(raw), err
}
