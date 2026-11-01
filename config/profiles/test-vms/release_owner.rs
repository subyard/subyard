//! Test-yard registration migration policy. The engine supplies durable execution.
use crate::{
    program::{Action, Operation::*, Program, Step},
    record::require,
    work::OwnerBefore,
    Result,
};

pub(crate) fn program(before: &OwnerBefore) -> Result<Program> {
    let adopted = before.state.ends_with("-adopt-current");
    let capture = before.capture.as_ref().ok_or(crate::Error::Invalid)?;
    require(matches!(
        capture.instance.as_str(),
        "absent" | "running" | "stopped" | "adopted"
    ))?;
    require(adopted == (capture.instance == "adopted"))?;
    let stopped = capture.instance == "stopped";
    let action = |operation, slot| Action::new(operation, slot);
    let mut preflight = vec![];
    if stopped {
        preflight.push(action(RestoreStopped, 1));
    }
    preflight.push(action(VerifyCompensated, if adopted { 0 } else { 1 }));
    let mut rollback = vec![action(RestoreRegistration, 0)];
    if !adopted {
        rollback.extend([action(Teardown, 0), action(RestoreOverrides, 0)]);
    }
    rollback.push(action(ParkCurrentRegistration, 0));
    if !adopted {
        rollback.extend([action(Project, 2), action(Init, 2), action(Check, 1)]);
        if stopped {
            rollback.push(action(RestoreStopped, 2));
        }
    }
    rollback.push(action(VerifyCompensated, if adopted { 0 } else { 2 }));
    let mut forward = vec![];
    let mut add = |operation, slot, compensation| {
        forward.push(Step {
            action: action(operation, slot),
            compensation,
        });
    };
    add(ValidateOriginal, 0, Some(0));
    if !adopted {
        if capture.instance == "absent" {
            // A partial restoration has original create custody and resumes
            // that same init; it has not yet reached the legacy preflight.
            add(Init, 1, None);
        }
        if stopped {
            add(Power, 1, Some(0));
            add(Start, 1, Some(0));
        }
        add(Check, 1, Some(0));
        add(CheckLease, 1, Some(0));
        if stopped {
            add(RestoreStopped, 1, Some(0));
        } else if capture.instance != "absent" {
            // Restoration init already installs host power; the original
            // absent capture has no independent host-power capsule.
            add(Power, 1, Some(0));
        }
    }
    add(PublishRegistration, 0, None);
    if !adopted {
        add(Project, 0, Some(1));
        add(Teardown, 1, Some(1));
    }
    add(RemoveEmptyFlat, 0, Some(1));
    add(MoveOverrides, 0, Some(1));
    // Interrupted archival retains its exact resumable state; it must not
    // start compensation while a rename's outcome is unresolved.
    add(ArchiveRegistration, 0, None);
    if !adopted {
        add(Init, 0, Some(1));
    }
    add(Check, 0, Some(1));
    add(ArchiveController, 0, None);
    add(VerifyDesired, 0, None);
    Program::new(forward, vec![preflight, rollback])
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn original_capture_selects_preflight_and_compensation_without_native_replanning() {
        for instance in ["absent", "running", "stopped", "adopted"] {
            let before = OwnerBefore {
                state: if instance == "adopted" {
                    "legacy-flat-adopt-current"
                } else {
                    "legacy-flat"
                }
                .into(),
                capture: Some(crate::work::OwnerCapture {
                    instance: instance.into(),
                }),
                ..Default::default()
            };
            let p = program(&before).unwrap();
            for step in &p.forward {
                if matches!(
                    step.action.operation,
                    PublishRegistration | ArchiveRegistration | ArchiveController
                ) || step.action == Action::new(Init, 1)
                {
                    assert!(
                        step.compensation.is_none(),
                        "partial operation keeps its exact continuation"
                    );
                }
                if instance == "adopted" {
                    assert!(!matches!(
                        step.action.operation,
                        Init | Teardown | Project | Start
                    ));
                }
            }
            assert_eq!(
                p.forward.iter().any(|s| s.action == Action::new(Start, 1)),
                instance == "stopped"
            );
            if instance == "absent" {
                assert!(!p.forward.iter().any(|s| s.action == Action::new(Power, 1)));
            }
            assert_eq!(
                p.compensations[0]
                    .iter()
                    .any(|a| *a == Action::new(RestoreStopped, 1)),
                instance == "stopped"
            );
            assert_eq!(
                p.compensations[1]
                    .iter()
                    .any(|a| *a == Action::new(RestoreStopped, 2)),
                instance == "stopped"
            );
        }
    }
}
