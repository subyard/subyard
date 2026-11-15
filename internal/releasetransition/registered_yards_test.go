package releasetransition

import (
	"context"

	"github.com/Subyard/Subyard/internal/domain"
)

type dependencyRegisteredOwner struct {
	*nativeConsentOwner
	failure   error
	remaining int
}

func (owner *dependencyRegisteredOwner) Observe(ctx context.Context, releases ReleasePair, links ReleaseLinks) (V2ActivationObservation, error) {
	if owner.remaining != 0 {
		if owner.remaining > 0 {
			owner.remaining--
		}
		return V2ActivationObservation{}, owner.failure
	}
	return owner.nativeConsentOwner.Observe(ctx, releases, links)
}

type refusingRegisteredOwner struct {
	*v2TestReconciler
	refusal error
}

func (owner *refusingRegisteredOwner) Observe(ctx context.Context, releases ReleasePair, links ReleaseLinks) (V2ActivationObservation, error) {
	if owner.refusal != nil {
		return V2ActivationObservation{}, owner.refusal
	}
	return owner.v2TestReconciler.Observe(ctx, releases, links)
}

type registeredVerdict struct{ message string }

func (verdict *registeredVerdict) Error() string { return verdict.message }
func (verdict *registeredVerdict) ActivationDiagnostic() (string, string) {
	return verdict.message, "correct the reported guest state, then run yard update --check"
}
func (*registeredVerdict) MigrationRefusal() bool { return true }

type nativeConsentOwner struct {
	*v2TestReconciler
	decline bool
	retired func(JournalRecord) bool
}

func (owner *nativeConsentOwner) RetiredJournalBinding(_ context.Context, journal JournalRecord) (bool, error) {
	return owner.retired != nil && owner.retired(journal), nil
}

func (owner *nativeConsentOwner) Reconcile(ctx context.Context, links ReleaseLinks) error {
	if owner.decline {
		return domain.ErrOperationDeclined
	}
	return owner.v2TestReconciler.Reconcile(ctx, links)
}

type migrationActionVerdict struct{ message, retry string }

func (failure migrationActionVerdict) Error() string          { return failure.message }
func (failure migrationActionVerdict) MigrationRefusal() bool { return true }
func (failure migrationActionVerdict) ActivationDiagnostic() (string, string) {
	return failure.message, failure.retry
}
