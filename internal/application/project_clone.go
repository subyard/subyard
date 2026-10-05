package application

import (
	"context"
	"errors"
	"fmt"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
)

// CheckEmptyCloneSource keeps an approved unborn clone from acquiring new refs.
// A missing HEAD alone does not prove that the repository is empty.
func CheckEmptyCloneSource(ctx context.Context, data ports.YardExecutor, yard domain.Context, source string) error {
	dev := uint32(yard.DevUID)
	result, err := data.Execute(ctx, yard, ports.InstanceExecRequest{
		Command:     []string{"git", "ls-remote", "--", source},
		Environment: map[string]string{"HOME": "/home/" + yard.DevUser}, User: dev, Group: dev,
	})
	if err != nil || result.ExitCode != 0 || len(result.Stdout) != 0 {
		return fmt.Errorf("%w: prepared empty clone source is unavailable or has refs: %w", domain.ErrPlanStale, errors.Join(err, errors.New("empty ref advertisement required")))
	}
	return nil
}

func (runner ProjectActionRunner) verifyUnbornClone(ctx context.Context) error {
	dev := uint32(runner.Yard.DevUID)
	result, err := runner.Data.Execute(ctx, runner.Yard, ports.InstanceExecRequest{
		Command: []string{"sh", "-eu", "-c", `
test -d "$1/.git" && test ! -L "$1/.git"
test "$(git -C "$1" rev-parse --is-inside-work-tree)" = true
head="$(git -C "$1" symbolic-ref -q HEAD)"
case "$head" in refs/heads/*) ;; *) exit 1 ;; esac
git check-ref-format "$head"
refs="$(git -C "$1" for-each-ref --format='%(refname)')"
test -z "$refs"
if git -C "$1" rev-parse --verify HEAD >/dev/null 2>&1; then exit 1; fi
files="$(find "$1" -mindepth 1 -maxdepth 1 ! -name .git -print -quit)"
test -z "$files"
`, "subyard", runner.Project.YardPath},
		User: dev, Group: dev,
	})
	if err != nil || result.ExitCode != 0 {
		return fmt.Errorf("%w: verify empty cloned repository: %w", domain.ErrPlanStale, errors.Join(err, errors.New("clone is not an empty unborn working repository")))
	}
	return nil
}
