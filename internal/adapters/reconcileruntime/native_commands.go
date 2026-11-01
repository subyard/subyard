package reconcileruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"

	"github.com/Subyard/Subyard/internal/ports"
)

type readOnlyCommandKey struct{}

// Only fixed observation callers select this mode; arguments never imply it.
func readOnlyCommand(ctx context.Context) context.Context {
	return context.WithValue(ctx, readOnlyCommandKey{}, true)
}

// beginNativeCommand observes an existing fixed component transport. Its
// foreground completion is distinct from an SDK UUID and cannot be recovered
// from desired state after transport loss.
func (runtime Runtime) beginNativeCommand(ctx context.Context, kind, path string, environment, arguments []string) (func(error) error, error) {
	finish, _, err := runtime.beginNativeCommandOrdinal(ctx, kind, path, environment, arguments)
	return finish, err
}
func (runtime Runtime) beginNativeCommandOrdinal(ctx context.Context, kind, path string, environment, arguments []string) (func(error) error, uint64, error) {
	observer := ports.NativeOperationObserverFrom(ctx)
	if observer == nil {
		return func(err error) error { return err }, 0, nil
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	boundArguments := append([]string(nil), arguments...)
	for index := range boundArguments {
		boundArguments[index] = runtime.bindingPath(boundArguments[index])
	}
	raw, err := json.Marshal(struct {
		Payload                []byte
		Environment, Arguments []string
	}{payload, runtime.bindingEnvironment(environment), boundArguments})
	if err != nil {
		return nil, 0, err
	}
	ordinal, err := observer.CommandStarted(kind, fmt.Sprintf("%x", sha256.Sum256(raw)))
	if err != nil {
		return nil, 0, err
	}
	return func(err error) error {
		if err != nil {
			observer.Uncertain()
			return err
		}
		return observer.CommandTerminal(ordinal)
	}, ordinal, nil
}
