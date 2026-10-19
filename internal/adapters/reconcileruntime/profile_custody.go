package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/Subyard/Subyard/internal/profile"
)

func validCustodyGeneration(value string) bool {
	return value == "0" || profileRuntimeDigest.MatchString(value)
}

func decodeCustodyGeneration(raw []byte) (string, error) {
	var value struct {
		Generation string `json:"generation"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || !validCustodyGeneration(value.Generation) {
		return "", errors.New("invalid native custody observation")
	}
	return value.Generation, nil
}

func (runtime Runtime) custodyDefinition(id string) (profile.Definition, bool, error) {
	definitions, err := runtime.profileRuntimeDefinitions()
	if err != nil {
		return profile.Definition{}, false, err
	}
	for _, definition := range definitions {
		if definition.Runtime.ActivationID == id {
			return definition, definition.Runtime.Custody, nil
		}
	}
	return profile.Definition{}, false, errors.New("profile runtime owner unavailable")
}

func (runtime Runtime) CaptureProfileCustody(ctx context.Context, id string) (string, bool, error) {
	definition, capable, err := runtime.custodyDefinition(id)
	if err != nil || !capable {
		return "", capable, err
	}
	stdout, _, err := runtime.runProfileRuntimeHandler(ctx, definition, "custody-observe")
	if err != nil {
		return "", true, err
	}
	generation, err := decodeCustodyGeneration(stdout)
	return generation, true, err
}

func (runtime Runtime) ApplyProfileRuntimeCustody(ctx context.Context, id, operation, actual, desired, expected, generation string) error {
	definition, capable, err := runtime.custodyDefinition(id)
	if err != nil {
		return err
	}
	if !capable || !validCustodyGeneration(expected) || !profileRuntimeDigest.MatchString(generation) {
		return errors.New("native profile custody unsupported")
	}
	return runtime.applyProfileRuntime(ctx, definition, operation, actual, desired, expected, generation)
}

func (runtime Runtime) FenceProfileRuntime(ctx context.Context, id, expected, generation, successor string) error {
	definition, capable, err := runtime.custodyDefinition(id)
	if err != nil {
		return err
	}
	if !capable || !validCustodyGeneration(expected) || !profileRuntimeDigest.MatchString(generation) || !profileRuntimeDigest.MatchString(successor) || successor == generation || successor == expected {
		return errors.New("invalid native profile fence")
	}
	stdout, _, err := runtime.runProfileRuntimeHandler(ctx, definition, "custody-fence", expected, generation, successor)
	if err != nil {
		return err
	}
	current, err := decodeCustodyGeneration(stdout)
	if err != nil || current != successor {
		return errors.New("native profile fence was not verified")
	}
	return nil
}
