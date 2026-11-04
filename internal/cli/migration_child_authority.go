package cli

import (
	"context"
	"errors"
	"slices"

	"github.com/Subyard/Subyard/internal/testyardmigration"
)

// The environment marker only routes prompts. Hidden migration effects require
// this private value, which only their in-process owning parents can set.
type migrationChildAuthorityKey struct{}
type migrationChildAuthority struct{ action, yard string }

func ownerMigrationChildContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, migrationChildAuthorityKey{}, migrationChildAuthority{"reconcile-power-reconciler", testyardmigration.LegacyYard})
}

func routeMigrationChildContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, migrationChildAuthorityKey{}, migrationChildAuthority{"reconcile-test-vm-broker", testyardmigration.CurrentYard})
}

func migrationHelperAction(action string) bool {
	return action == "reconcile-power-reconciler" || action == "reconcile-test-vm-broker"
}

func requireMigrationChildAuthority(ctx context.Context, yard, action string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ctx.Value(migrationChildAuthorityKey{}) != (migrationChildAuthority{action, yard}) {
		return errors.New("migration child requires its owning parent")
	}
	return nil
}

// Only the canonical hidden tuple may use the delegated authority. Global yard
// selectors embedded in child arguments cannot retarget its separate yard.
func validateMigrationChildInvocation(ctx context.Context, yard string, arguments []string) error {
	selected, _, yes, remaining, err := parseGlobals(arguments, yard)
	if err != nil {
		return err
	}
	if len(remaining) < 2 || remaining[0] != "_migrate" || !migrationHelperAction(remaining[1]) {
		return nil
	}
	if yes || selected != yard || !slices.Equal(arguments, []string{"_migrate", remaining[1]}) {
		return errors.New("migration child arguments differ from the authorized tuple")
	}
	return requireMigrationChildAuthority(ctx, yard, remaining[1])
}
