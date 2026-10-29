//! Frozen release metadata assembled from one immutable component capture.
use crate::{codec, digest, native::Target, record::require, Result};
use serde::Serialize;
use std::collections::BTreeMap;
pub(crate) struct Assessment {
    pub binding: String,
    pub actual: String,
    pub desired: String,
    pub converged: bool,
}
pub(crate) fn assess(
    id: &str,
    target: &str,
    artifact: &str,
    active: &str,
    targets: &[Target],
    source_complete: bool,
) -> Result<Assessment> {
    let mut converged = targets.iter().all(|t| t.state != "stale");
    let (desired, actual) = match id {
        "installation-runtime-v1" => {
            require(targets.len() == 1 && targets[0].class == "installation")?;
            require(
                crate::record::fingerprint(&targets[0].reported_actual)
                    && crate::record::fingerprint(&targets[0].reported_desired),
            )?;
            (
                targets[0].reported_desired.clone(),
                targets[0].reported_actual.clone(),
            )
        }
        "host-power" | "test-vm-broker" => {
            require(targets.len() == 1 && targets[0].class == "stage")?;
            let t = &targets[0];
            #[derive(Serialize)]
            struct Desired<'a> {
                id: &'a str,
                stage: &'a str,
                state: &'a str,
                target: &'a str,
            }
            #[derive(Serialize)]
            struct Actual<'a> {
                id: &'a str,
                stage: &'a str,
                state: &'a str,
                active: &'a str,
                drift: bool,
            }
            let desired = digest(&codec::encode(
                &Desired {
                    id,
                    stage: &t.stage,
                    state: &t.scope_state,
                    target,
                },
                false,
            )?);
            let actual = digest(&codec::encode(
                &Actual {
                    id,
                    stage: &t.stage,
                    state: &t.scope_state,
                    active,
                    drift: true,
                },
                false,
            )?);
            (desired, actual)
        }
        "test-yard-route-consumers" => {
            require(targets.len() == 1 && targets[0].class == "route-consumers")?;
            let t = &targets[0];
            #[derive(Serialize)]
            struct Desired<'a> {
                id: &'a str,
                target: &'a str,
                active: bool,
            }
            #[derive(Serialize)]
            struct Owner<'a> {
                id: &'a str,
                owner: &'a str,
            }
            #[derive(Serialize)]
            struct Drift<'a> {
                id: &'a str,
                before: &'a str,
                drift: bool,
            }
            let desired = digest(&codec::encode(
                &Desired {
                    id,
                    target,
                    active: t.scope_state != "absent",
                },
                false,
            )?);
            let actual = if t.scope_state != "current" {
                digest(&codec::encode(
                    &Owner {
                        id,
                        owner: &t.scope_state,
                    },
                    false,
                )?)
            } else {
                digest(&codec::encode(
                    &Drift {
                        id,
                        before: &t.before,
                        drift: true,
                    },
                    false,
                )?)
            };
            (desired, actual)
        }
        "materialized-config" => {
            #[derive(Serialize)]
            struct Fingerprint<'a> {
                name: &'a str,
                fingerprint: String,
                #[serde(skip_serializing_if = "str::is_empty")]
                state: &'a str,
                #[serde(skip_serializing_if = "str::is_empty")]
                integrations: &'a str,
            }
            let mut grouped = BTreeMap::<&str, (Option<&Target>, Option<&Target>)>::new();
            for t in targets {
                let pair = grouped.entry(&t.yard).or_default();
                match t.class.as_str() {
                    "materialized-files" => {
                        require(pair.0.is_none())?;
                        pair.0 = Some(t)
                    }
                    "materialized-integrations" => {
                        require(pair.1.is_none())?;
                        pair.1 = Some(t)
                    }
                    _ => return Err(crate::Error::Invalid),
                }
            }
            let mut desired = vec![];
            let mut actual = vec![];
            for (yard, (file, integration)) in grouped {
                let f = file.ok_or(crate::Error::Invalid)?;
                let i = integration
                    .map(|i| i.reported_actual.as_str())
                    .unwrap_or("");
                let scope = f.scope.as_str();
                desired.push(Fingerprint {
                    name: yard,
                    fingerprint: f.reported_desired.clone(),
                    state: "",
                    integrations: scope,
                });
                actual.push(Fingerprint {
                    name: yard,
                    fingerprint: digest(&codec::encode(&[f.reported_actual.as_str(), i], false)?),
                    state: &f.scope_state,
                    integrations: "",
                });
            }
            (
                digest(&codec::encode(&desired, false)?),
                digest(&codec::encode(&actual, false)?),
            )
        }
        _ => {
            require(targets.iter().all(|t| t.class == "profile-runtime"))?;
            #[derive(Serialize)]
            struct Desired<'a> {
                id: &'a str,
                target: &'a str,
                artifact: &'a str,
            }
            #[derive(Serialize)]
            #[serde(rename_all = "PascalCase")]
            struct Actual<'a> {
                yard: &'a str,
                project: &'a str,
                instance: &'a str,
                state: &'a str,
                digest: &'a str,
                desired: &'a str,
            }
            let values = targets
                .iter()
                .map(|t| Actual {
                    yard: &t.yard,
                    project: &t.project,
                    instance: &t.instance,
                    state: if matches!(t.state.as_str(), "current" | "stale") {
                        "installed"
                    } else {
                        &t.state
                    },
                    digest: &t.reported_actual,
                    desired: &t.reported_desired,
                })
                .collect::<Vec<_>>();
            #[derive(Serialize)]
            struct Pending<'a> {
                id: &'a str,
                state: &'a str,
            }
            let actual = if source_complete {
                digest(&codec::encode(&values, false)?)
            } else {
                converged = false;
                digest(&codec::encode(
                    &Pending {
                        id,
                        state: "source-migrations-pending",
                    },
                    false,
                )?)
            };
            (
                digest(&codec::encode(
                    &Desired {
                        id,
                        target,
                        artifact,
                    },
                    false,
                )?),
                actual,
            )
        }
    };
    Ok(Assessment {
        binding: if id == "installation-runtime-v1" {
            targets[0].source.clone()
        } else {
            String::new()
        },
        actual: if converged { desired.clone() } else { actual },
        desired,
        converged,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde::Deserialize;
    #[derive(Deserialize)]
    struct Expected {
        #[serde(default)]
        binding: String,
        actual: String,
        desired: String,
        converged: bool,
    }
    #[derive(Deserialize)]
    struct Case {
        owner: String,
        target: String,
        artifact: String,
        active: String,
        targets: Vec<Target>,
        expected: Expected,
        #[serde(default, rename = "sourcePending")]
        source_pending: bool,
    }
    #[test]
    fn owning_go_same_activation_facts() {
        let Ok(path) = std::env::var("SUBYARD_RUST_ACTIVATION_CORPUS") else {
            return;
        };
        let cases: Vec<Case> = serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
        assert_eq!(cases.len(), 35);
        for (n, c) in cases.into_iter().enumerate() {
            let value = assess(
                &c.owner,
                &c.target,
                &c.artifact,
                &c.active,
                &c.targets,
                !c.source_pending,
            )
            .unwrap();
            assert_eq!(value.actual, c.expected.actual, "case {n} actual");
            assert_eq!(value.desired, c.expected.desired, "case {n} desired");
            assert_eq!(value.binding, c.expected.binding, "case {n} binding");
            assert_eq!(value.converged, c.expected.converged, "case {n} converged")
        }
    }
}
