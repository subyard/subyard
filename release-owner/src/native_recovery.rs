//! Exact invocation continuation. A failed operation is quiescent, not success;
//! an opaque lost reply needs a resource-side fence before another apply.
use crate::{
    codec,
    custody::Intent,
    digest, inventory,
    lease::UpdateLease,
    native::{self, Binding, Request},
    owner::Owner,
    record::{self, require},
    store::{self, Address},
    wire::journal_v2 as j,
    Error, Result,
};
use serde::{Deserialize, Serialize};

#[derive(Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
struct ResourceGeneration {
    role: String,
    value: String,
}
#[derive(Clone, Serialize, Deserialize)]
#[serde(untagged)]
enum Expected {
    Scalar(String),
    Vector(Vec<ResourceGeneration>),
}
impl From<String> for Expected {
    fn from(value: String) -> Self {
        Self::Scalar(value)
    }
}
#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Generation {
    version: u8,
    expected: Expected,
    generation: String,
}
impl Generation {
    fn values(&self) -> Vec<&str> {
        match &self.expected {
            Expected::Scalar(s) => vec![s.as_str()],
            Expected::Vector(v) => v.iter().map(|r| r.value.as_str()).collect(),
        }
    }
    fn parse(raw: &str) -> Result<Self> {
        let value: Self = codec::decode_canonical(raw.as_bytes(), false)?;
        require(
            record::fingerprint(&value.generation)
                && value
                    .values()
                    .iter()
                    .all(|v| (*v == "0" || record::fingerprint(v)) && *v != value.generation),
        )?;
        match &value.expected {
            Expected::Scalar(_) => require(value.version == 1)?,
            Expected::Vector(v) => require(
                value.version == 2
                    && (v.iter().map(|r| r.role.as_str()).eq(["engine", "service"])
                        || v.iter()
                            .map(|r| r.role.as_str())
                            .eq(["engine", "service", "timer"])),
            )?,
        }
        Ok(value)
    }
    fn replacement(&self, successor: &str, generation: String) -> Self {
        let expected = match &self.expected {
            Expected::Scalar(_) => Expected::Scalar(successor.into()),
            Expected::Vector(v) => Expected::Vector(
                v.iter()
                    .map(|r| ResourceGeneration {
                        role: r.role.clone(),
                        value: successor.into(),
                    })
                    .collect(),
            ),
        };
        Self {
            version: self.version,
            expected,
            generation,
        }
    }
    fn matches_replacement(&self, after: &Self, successor: &str) -> Result<()> {
        require(
            record::fingerprint(successor)
                && successor != self.generation
                && !self.values().contains(&successor)
                && after.generation != self.generation
                && !self.values().contains(&after.generation.as_str())
                && codec::encode(
                    &self.replacement(successor, after.generation.clone()),
                    false,
                )? == codec::encode(after, false)?,
        )
    }
}
#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Fence {
    binding: Binding,
    target: native::Target,
    successor: String,
    replacement: native::Target,
}
#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct Resolution {
    version: u8,
    binding: Binding,
    intent: String,
    retry: Binding,
    fence: Option<Fence>,
}
#[derive(Serialize)]
#[serde(deny_unknown_fields)]
struct Fenced<'a> {
    version: u8,
    expected: &'a str,
    generation: &'a str,
    successor: &'a str,
    current: &'a str,
    source: &'a str,
}
fn same_binding(old: &Binding, new: &Binding) -> bool {
    let mut projected = new.clone();
    projected.ordinal = old.ordinal;
    projected == *old && new.ordinal > 2048 && new.ordinal <= 4096 && new.ordinal > old.ordinal
}
impl Resolution {
    pub(crate) fn checked(raw: &[u8], original: &Intent, intent_raw: &[u8]) -> Result<Self> {
        Self::checked_hash(raw, original, &digest(intent_raw))
    }
    pub(crate) fn checked_hash(raw: &[u8], original: &Intent, intent_hash: &str) -> Result<Self> {
        let value: Self = codec::decode_canonical(raw, true)?;
        require(
            value.version == 1
                && value.binding == original.binding
                && value.intent == intent_hash
                && same_binding(&value.binding, &value.retry),
        )?;
        if let Some(fence) = &value.fence {
            require(
                same_binding(&value.binding, &fence.binding)
                    && fence.binding.ordinal != value.retry.ordinal
                    && matches!(fence.target.class.as_str(), "profile-runtime" | "stage")
                    && fence.target.owner == original.owner
                    && fence.target.yard == original.resource
                    && fence.target.actual == original.expected
                    && fence.target.desired == original.desired
                    && fence.target.source == original.source
                    && digest(&codec::encode(&fence.target, false)?) == original.recipe,
            )?;
            fence.target.validate()?;
            let before = Generation::parse(&fence.target.before)?;
            let after = Generation::parse(&fence.replacement.before)?;
            before.matches_replacement(&after, &fence.successor)?;
            let mut target = fence.target.clone();
            target.before = fence.replacement.before.clone();
            require(codec::encode(&target, false)? == codec::encode(&fence.replacement, false)?)?;
        }
        Ok(value)
    }
    fn observed(&self) -> Result<String> {
        let fence = self.fence.as_ref().ok_or(Error::Invalid)?;
        let before = Generation::parse(&fence.target.before)?;
        let raw = match &before.expected {
            Expected::Scalar(expected) => codec::encode(
                &Fenced {
                    version: 1,
                    expected,
                    generation: &before.generation,
                    successor: &fence.successor,
                    current: &fence.successor,
                    source: &fence.target.source,
                },
                false,
            )?,
            Expected::Vector(expected) => {
                #[derive(Serialize)]
                struct Observed<'a> {
                    version: u8,
                    expected: &'a [ResourceGeneration],
                    generation: &'a str,
                    successor: &'a str,
                    current: Vec<ResourceGeneration>,
                    source: &'a str,
                }
                codec::encode(
                    &Observed {
                        version: 2,
                        expected,
                        generation: &before.generation,
                        successor: &fence.successor,
                        current: expected
                            .iter()
                            .map(|r| ResourceGeneration {
                                role: r.role.clone(),
                                value: fence.successor.clone(),
                            })
                            .collect(),
                        source: &fence.target.source,
                    },
                    false,
                )?
            }
        };
        String::from_utf8(raw).map_err(|_| Error::Codec)
    }
    fn retry_recipe(&self, original: &Intent) -> Result<String> {
        self.fence
            .as_ref()
            .map(|f| codec::encode(&f.replacement, false).map(|raw| digest(&raw)))
            .unwrap_or_else(|| Ok(original.recipe.clone()))
    }
    pub(crate) fn proof(
        &self,
        original: &Intent,
        intents: &std::collections::BTreeMap<u64, Intent>,
        terminals: &std::collections::BTreeMap<u64, native::Terminal>,
        completed: &std::collections::BTreeMap<u64, u64>,
        admitting: Option<&Binding>,
    ) -> Result<(bool, bool)> {
        let matches =
            |binding: &Binding, recipe: &str, expected: &str, desired: &str| -> Result<bool> {
                let Some(intent) = intents.get(&binding.ordinal) else {
                    return Ok(false);
                };
                require(
                    intent.binding == *binding
                        && intent.owner == original.owner
                        && intent.resource == original.resource
                        && intent.source == original.source
                        && intent.recipe == recipe
                        && intent.expected == expected
                        && intent.desired == desired,
                )?;
                Ok(completed.contains_key(&binding.ordinal))
            };
        let success = matches(
            &self.retry,
            &self.retry_recipe(original)?,
            &original.expected,
            &original.desired,
        )?;
        if let Some(fence) = &self.fence {
            let request = Request::FenceActivation {
                binding: fence.binding.clone(),
                target: fence.target.clone(),
                successor: fence.successor.clone(),
            };
            let observed = self.observed()?;
            let quiet = matches(
                &fence.binding,
                &request.recipe_fingerprint()?,
                &digest(fence.target.before.as_bytes()),
                &digest(observed.as_bytes()),
            )?;
            if quiet {
                let terminal = terminals
                    .get(
                        completed
                            .get(&fence.binding.ordinal)
                            .ok_or(Error::Unknown)?,
                    )
                    .ok_or(Error::Unknown)?;
                require(
                    terminal.observed == observed
                        && terminal.project == fence.target.project
                        && terminal.instance == fence.target.instance
                        && terminal.instance_uuid == fence.target.instance_uuid,
                )?;
            }
            return Ok((
                quiet || admitting.is_some_and(|b| *b == fence.binding),
                success && quiet,
            ));
        }
        require(
            terminals
                .get(&original.binding.ordinal)
                .is_some_and(|t| t.state == "failed"),
        )?;
        Ok((false, success))
    }
    pub(crate) fn completed_retry(
        &self,
        completed: &std::collections::BTreeMap<u64, u64>,
    ) -> Option<u64> {
        completed.get(&self.retry.ordinal).copied()
    }
}
impl store::Store {
    /// Select only the original invocation or its explicit protected retry
    /// edges. Directory order and resource names never select a producer.
    pub(crate) fn owner_result_producers(
        &self,
        base: &Binding,
        request: &Request,
        expected: &str,
        desired: &str,
        source: &str,
    ) -> Result<Vec<Binding>> {
        match request {
            Request::ApplyOwner {
                binding,
                before,
                token,
            } => {
                require(
                    binding == base
                        && token.len() == 32
                        && token
                            .bytes()
                            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b)),
                )?;
                before.validate()?;
            }
            _ => return Err(Error::Invalid),
        }
        let recipe = request.recipe_fingerprint()?;
        let mut binding = base.clone();
        let mut chain = Vec::new();
        loop {
            binding.validate()?;
            let read = |kind| {
                self.read(&Address::native(
                    &binding.transaction,
                    binding.ordinal,
                    kind,
                )?)
            };
            let raw = read("intent")?;
            let accepted = read("accepted")?;
            let terminal = read("terminal")?;
            let Some(bytes) = raw.bytes() else {
                require(accepted.bytes().is_none() && terminal.bytes().is_none())?;
                return Ok(chain);
            };
            let intent: Intent = codec::decode_canonical(bytes, true)?;
            require(
                intent.version == 1
                    && intent.binding == binding
                    && intent.owner == "owner-registration"
                    && intent.resource == "test-yard"
                    && intent.recipe == recipe
                    && intent.expected == expected
                    && intent.desired == desired
                    && intent.source == source,
            )?;
            let Some(accepted) = accepted.bytes() else {
                require(terminal.bytes().is_none())?;
                return Ok(chain);
            };
            let accepted: native::Terminal = codec::decode_canonical(accepted, true)?;
            require(
                accepted.version == 1
                    && accepted.binding == binding
                    && accepted.owner == intent.owner
                    && accepted.resource == intent.resource
                    && accepted.state == "accepted",
            )?;
            let terminal: native::Terminal =
                codec::decode_canonical(terminal.bytes().ok_or(Error::Unknown)?, true)?;
            require(
                terminal.version == 1
                    && terminal.binding == binding
                    && terminal.owner == intent.owner
                    && terminal.resource == intent.resource
                    && matches!(terminal.state.as_str(), "complete" | "failed"),
            )?;
            self.validate_owner_producer(&binding)?;
            chain.push(binding.clone());
            let recovery = read("recovery")?;
            if terminal.state == "complete" || recovery.bytes().is_none() {
                return Ok(chain);
            }
            let recovery =
                Resolution::checked(recovery.bytes().ok_or(Error::Unknown)?, &intent, bytes)?;
            require(recovery.fence.is_none())?;
            binding = recovery.retry;
        }
    }
}
impl Owner {
    pub(crate) fn needs_native_continuation(
        &self,
        journal: &j::Record,
        ordinal: u64,
    ) -> Result<bool> {
        let accepted =
            self.store
                .read(&Address::native(&journal.transaction, ordinal, "accepted")?)?;
        if accepted.bytes().is_none() {
            return Ok(false);
        }
        let terminal =
            self.store
                .read(&Address::native(&journal.transaction, ordinal, "terminal")?)?;
        let terminal = terminal
            .bytes()
            .map(|raw| codec::decode_canonical::<native::Terminal>(raw, true))
            .transpose()?;
        Ok(!terminal.is_some_and(|t| t.state == "complete"))
    }
    pub(crate) fn continued_native_request(
        &self,
        lease: &UpdateLease<'_>,
        journal: &j::Record,
        binding: Binding,
        request: Request,
    ) -> Result<(Binding, Request)> {
        let read = |kind| {
            self.store.read(&Address::native(
                &binding.transaction,
                binding.ordinal,
                kind,
            )?)
        };
        if read("accepted")?.bytes().is_none() {
            return Ok((binding, request));
        }
        let terminal_raw = read("terminal")?;
        let terminal = terminal_raw
            .bytes()
            .map(|raw| codec::decode_canonical::<native::Terminal>(raw, true))
            .transpose()?;
        if terminal.as_ref().is_some_and(|t| t.state == "complete") {
            return Ok((binding, request));
        }
        if terminal.is_none() {
            if let Request::CleanupOwner { before, token, .. } = &request {
                let update = native::Identity::capture(&lease._lock)?;
                let (owner_identity, owner_fd) = self.outer.borrowed()?;
                self.native(
                    &Request::ClaimLease {
                        binding: binding.clone(),
                        update,
                        owner: Some(owner_identity),
                    },
                    vec![store::io(rustix::io::dup(&lease._lock))?, owner_fd],
                )?;
                let queried = self.call(Request::QueryOwnerCleanup {
                    binding: binding.clone(),
                    before: before.clone(),
                    token: token.clone(),
                });
                if self.store.reader.health().is_ok() {
                    self.call(Request::ReleaseLease {
                        session: self.session.clone(),
                    })?;
                }
                queried?;
                let raw = read("terminal")?;
                let fact: native::Terminal =
                    codec::decode_canonical(raw.bytes().ok_or(Error::Unknown)?, true)?;
                require(
                    matches!(fact.state.as_str(), "complete" | "failed")
                        && fact.binding == binding
                        && fact.owner == "owner-registration-cleanup"
                        && fact.resource == "test-yard",
                )?;
                return self.continued_native_request(lease, journal, binding, request);
            }
        }
        let intent_raw = read("intent")?;
        let intent: Intent =
            codec::decode_canonical(intent_raw.bytes().ok_or(Error::Unknown)?, true)?;
        require(intent.binding == binding && intent.recipe == request.recipe_fingerprint()?)?;
        let address = Address::native(&binding.transaction, binding.ordinal, "recovery")?;
        let raw = self.store.read(&address)?;
        let resolution = if let Some(raw) = raw.bytes() {
            Resolution::checked(raw, &intent, intent_raw.bytes().ok_or(Error::Unknown)?)?
        } else {
            let known_failed = terminal.as_ref().is_some_and(|t| t.state == "failed");
            if known_failed {
                self.store
                    .validate_native_transaction(&binding.transaction)?;
            }
            let target = if !known_failed {
                match &request {
                    Request::ApplyActivation { target, .. }
                        if matches!(target.class.as_str(), "profile-runtime" | "stage") =>
                    {
                        Generation::parse(&target.before).map_err(|_| Error::Unknown)?;
                        Some(target.clone())
                    }
                    _ => {
                        // Query may add exact-ID terminal witnesses, never infer a missing
                        // whole-invocation terminal or replay an opaque accepted request.
                        let update = native::Identity::capture(&lease._lock)?;
                        let (owner_identity, owner_fd) = self.outer.borrowed()?;
                        self.native(
                            &Request::ClaimLease {
                                binding: binding.clone(),
                                update,
                                owner: Some(owner_identity),
                            },
                            vec![store::io(rustix::io::dup(&lease._lock))?, owner_fd],
                        )?;
                        let queried = self.call(Request::QueryInvocation {
                            binding: binding.clone(),
                        });
                        if self.store.reader.health().is_ok() {
                            self.call(Request::ReleaseLease {
                                session: self.session.clone(),
                            })?;
                        }
                        let _ = queried;
                        return Err(Error::Unknown);
                    }
                }
            } else {
                None
            };
            let root = self.store.root.as_ref().ok_or(Error::Protected)?;
            let dir = store::directory(
                root,
                &[
                    "migrations".into(),
                    "native-custody".into(),
                    "v1".into(),
                    binding.transaction.clone(),
                ],
                false,
            )?
            .ok_or(Error::Stale)?;
            let last = inventory::names(&dir, 8192)?
                .iter()
                .filter_map(|n| n.split_once('.').and_then(|(n, _)| n.parse::<u64>().ok()))
                .max()
                .unwrap_or(2048)
                .max(2048);
            let mut retry = binding.clone();
            retry.ordinal = last.checked_add(1).ok_or(Error::Bound)?;
            let reserved = self.cleanup_owner_ordinal(journal)?;
            if reserved == Some(retry.ordinal) {
                retry.ordinal = retry.ordinal.checked_add(1).ok_or(Error::Bound)?;
            }
            let fence = if let Some(target) = target {
                let fence_binding = retry.clone();
                retry.ordinal = retry.ordinal.checked_add(1).ok_or(Error::Bound)?;
                let mut entropy = [0; 32];
                {
                    use std::io::Read;
                    std::fs::File::open("/dev/urandom")
                        .and_then(|mut f| f.read_exact(&mut entropy))
                        .map_err(|_| Error::Io)?;
                }
                let successor = digest(&entropy);
                let mut replacement = target.clone();
                replacement.before = String::from_utf8(codec::encode(
                    &Generation::parse(&target.before)?
                        .replacement(&successor, digest(&codec::encode(&retry, false)?)),
                    false,
                )?)
                .map_err(|_| Error::Codec)?;
                fence_binding.validate()?;
                Some(Fence {
                    binding: fence_binding,
                    target,
                    successor,
                    replacement,
                })
            } else {
                None
            };
            let value = Resolution {
                version: 1,
                binding: binding.clone(),
                intent: intent_raw.fingerprint(),
                retry,
                fence,
            };
            let raw = codec::encode(&value, true)?;
            let value =
                Resolution::checked(&raw, &intent, intent_raw.bytes().ok_or(Error::Unknown)?)?;
            lease.create_immutable(&address, &raw)?;
            value
        };
        if let Some(fence) = &resolution.fence {
            let observed = resolution.observed()?;
            self.dispatch(
                lease,
                journal,
                fence.binding.ordinal,
                crate::execution::Effect {
                    owner: &intent.owner,
                    resource: &intent.resource,
                    expected: &digest(fence.target.before.as_bytes()),
                    desired: &digest(observed.as_bytes()),
                    source: &intent.source,
                },
                |binding| Request::FenceActivation {
                    binding,
                    target: fence.target.clone(),
                    successor: fence.successor.clone(),
                },
            )?;
        }
        let request = if let Some(fence) = &resolution.fence {
            Request::ApplyActivation {
                binding: resolution.retry.clone(),
                target: fence.replacement.clone(),
            }
        } else {
            request.rebound(resolution.retry.clone())?
        };
        Ok((resolution.retry, request))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::custody::tests::{fixture, terminal, write, Fixture};
    #[test]
    fn owner_results_follow_only_original_closed_retry_edges() {
        let (f, s, _, base) = fixture();
        let before = crate::work::OwnerBefore {
            state: "legacy-flat".into(),
            registration: "a".repeat(64),
            overrides: "b".repeat(64),
            controller: "c".repeat(64),
            shared_images: false,
            prepared_recipe: "d".repeat(64),
        };
        let request = Request::ApplyOwner {
            binding: base.clone(),
            before: before.clone(),
            token: "e".repeat(32),
        };
        let recipe = request.recipe_fingerprint().unwrap();
        let expected = before.fingerprint().unwrap();
        let desired = before.desired().fingerprint().unwrap();
        let make = |binding: &Binding| Intent {
            version: 1,
            binding: binding.clone(),
            owner: "owner-registration".into(),
            resource: "test-yard".into(),
            expected: expected.clone(),
            desired: desired.clone(),
            source: before.controller.clone(),
            recipe: recipe.clone(),
        };
        assert!(s
            .owner_result_producers(&base, &request, &expected, &desired, &before.controller)
            .unwrap()
            .is_empty());
        let mut first = base.clone();
        first.ordinal = 2050;
        let mut second = base.clone();
        second.ordinal = 2051;
        for (binding, state) in [(&base, "failed"), (&first, "failed"), (&second, "complete")] {
            top(&f, binding, &make(binding), Some(state));
        }
        for (binding, retry) in [(&base, &first), (&first, &second)] {
            let intent = make(binding);
            write(
                &f,
                binding,
                &format!("{:03}.recovery.json", binding.ordinal),
                &Resolution {
                    version: 1,
                    binding: binding.clone(),
                    intent: digest(&codec::encode(&intent, true).unwrap()),
                    retry: retry.clone(),
                    fence: None,
                },
            );
        }
        for binding in [&base, &first, &second] {
            s.lock().unwrap().close_native(binding).unwrap();
        }
        let mut unrelated = base.clone();
        unrelated.ordinal = 3000;
        top(&f, &unrelated, &make(&unrelated), None);
        let directory =
            f.0.join("migrations/native-custody/v1")
                .join(&base.transaction);
        let snapshot = || {
            let mut files = std::fs::read_dir(&directory)
                .unwrap()
                .map(|entry| {
                    let entry = entry.unwrap();
                    (entry.file_name(), std::fs::read(entry.path()).unwrap())
                })
                .collect::<Vec<_>>();
            files.sort();
            files
        };
        let before_read = snapshot();
        let chain = s
            .owner_result_producers(&base, &request, &expected, &desired, &before.controller)
            .unwrap();
        assert_eq!(snapshot(), before_read);
        assert_eq!(
            chain.iter().map(|b| b.ordinal).collect::<Vec<_>>(),
            vec![1, 2050, 2051]
        );
        assert!(s
            .owner_result_producers(&base, &request, &expected, &desired, &"f".repeat(64))
            .is_err());
        let mut wrong = base.clone();
        wrong.transaction = "wrong-transaction".into();
        assert!(s
            .owner_result_producers(&wrong, &request, &expected, &desired, &before.controller)
            .is_err());
        let name =
            f.0.join("migrations/native-custody/v1")
                .join(&base.transaction)
                .join("2051.terminal.json");
        std::fs::remove_file(name).unwrap();
        assert_eq!(
            s.owner_result_producers(&base, &request, &expected, &desired, &before.controller)
                .err(),
            Some(Error::Unknown)
        );
    }
    fn top(f: &Fixture, binding: &Binding, intent: &Intent, state: Option<&str>) {
        write(
            f,
            binding,
            &format!("{:03}.reservation.json", binding.ordinal),
            &crate::native_retention::Reservation::new(binding),
        );
        write(
            f,
            binding,
            &format!("{:03}.intent.json", binding.ordinal),
            intent,
        );
        let mut accepted = terminal(binding, "accepted", vec![]);
        accepted.owner = intent.owner.clone();
        accepted.resource = intent.resource.clone();
        write(
            f,
            binding,
            &format!("{:03}.accepted.json", binding.ordinal),
            &accepted,
        );
        if let Some(state) = state {
            let mut t = accepted;
            t.state = state.into();
            write(
                f,
                binding,
                &format!("{:03}.terminal.json", binding.ordinal),
                &t,
            );
        }
    }
    fn intent(binding: &Binding, target: &native::Target, recipe: String) -> Intent {
        Intent {
            version: 1,
            binding: binding.clone(),
            owner: target.owner.clone(),
            resource: target.yard.clone(),
            expected: target.actual.clone(),
            desired: target.desired.clone(),
            source: target.source.clone(),
            recipe,
        }
    }
    fn target() -> native::Target {
        native::Target {
            prepared_recipe: String::new(),
            after_owner: String::new(),
            scope: String::new(),
            reported_actual: "a".repeat(64),
            reported_desired: "b".repeat(64),
            before: String::from_utf8(
                codec::encode(
                    &Generation {
                        version: 1,
                        expected: String::from("0").into(),
                        generation: "d".repeat(64),
                    },
                    false,
                )
                .unwrap(),
            )
            .unwrap(),
            class: "profile-runtime".into(),
            scope_state: "installed".into(),
            stage: String::new(),
            owner: "fixture-profile".into(),
            yard: "default".into(),
            project: "project".into(),
            instance: "yard".into(),
            instance_uuid: "instance-uuid".into(),
            state: "stale".into(),
            actual: "a".repeat(64),
            desired: "b".repeat(64),
            source: "c".repeat(64),
        }
    }
    #[test]
    fn only_exact_resource_fence_then_verified_retry_resolves_opaque_loss() {
        for version in [1, 2] {
            let (f, s, j, binding) = fixture();
            let mut target = target();
            if version == 2 {
                target.class = "stage".into();
                target.before = String::from_utf8(
                    codec::encode(
                        &Generation {
                            version: 2,
                            expected: Expected::Vector(vec![
                                ResourceGeneration {
                                    role: "engine".into(),
                                    value: "0".into(),
                                },
                                ResourceGeneration {
                                    role: "service".into(),
                                    value: "c".repeat(64),
                                },
                            ]),
                            generation: "d".repeat(64),
                        },
                        false,
                    )
                    .unwrap(),
                )
                .unwrap();
            }
            let original = intent(
                &binding,
                &target,
                digest(&codec::encode(&target, false).unwrap()),
            );
            top(&f, &binding, &original, None);
            let command = crate::custody::Command {
                kind: "profile-refresh".into(),
                source: target.source.clone(),
            };
            write(&f, &binding, "001.command-001-intent.json", &command);
            assert_eq!(s.native_inventory(Some(&j), false), Err(Error::Unknown));
            let mut fence_binding = binding.clone();
            fence_binding.ordinal = 2049;
            let mut retry = binding.clone();
            retry.ordinal = 2050;
            let successor = "e".repeat(64);
            let mut replacement = target.clone();
            replacement.before = String::from_utf8(
                codec::encode(
                    &Generation::parse(&target.before)
                        .unwrap()
                        .replacement(&successor, "f".repeat(64)),
                    false,
                )
                .unwrap(),
            )
            .unwrap();
            let resolution = Resolution {
                version: 1,
                binding: binding.clone(),
                intent: digest(&codec::encode(&original, true).unwrap()),
                retry: retry.clone(),
                fence: Some(Fence {
                    binding: fence_binding.clone(),
                    target: target.clone(),
                    successor: successor.clone(),
                    replacement: replacement.clone(),
                }),
            };
            write(&f, &binding, "001.recovery.json", &resolution);
            let observed = resolution.observed().unwrap();
            let mut fence_intent = intent(
                &fence_binding,
                &target,
                Request::FenceActivation {
                    binding: fence_binding.clone(),
                    target: target.clone(),
                    successor,
                }
                .recipe_fingerprint()
                .unwrap(),
            );
            fence_intent.expected = digest(target.before.as_bytes());
            fence_intent.desired = digest(observed.as_bytes());
            // The exact new intent is admitted for fencing, while ordinary admission
            // and Ready still reject the opaque old invocation.
            write(
                &f,
                &fence_binding,
                "2049.reservation.json",
                &crate::native_retention::Reservation::new(&fence_binding),
            );
            write(&f, &fence_binding, "2049.intent.json", &fence_intent);
            assert!(s.native_before_dispatch(&fence_binding).is_ok());
            assert!(s.native_inventory(Some(&j), true).is_err());
            top(&f, &fence_binding, &fence_intent, None);
            let mut fenced = terminal(&fence_binding, "complete", vec![]);
            fenced.owner = target.owner.clone();
            fenced.resource = target.yard.clone();
            fenced.project = target.project.clone();
            fenced.instance = target.instance.clone();
            fenced.instance_uuid = target.instance_uuid.clone();
            fenced.observed = observed;
            write(&f, &fence_binding, "2049.terminal.json", &fenced);
            write(&f, &fence_binding, "2049.command-001-intent.json", &command);
            assert!(s.native_inventory(Some(&j), false).is_err());
            write(
                &f,
                &fence_binding,
                "2049.command-001-terminal.json",
                &command,
            );
            assert!(s.native_inventory(Some(&j), false).is_ok());
            assert!(s.native_inventory(Some(&j), true).is_err());
            let retry_intent = intent(
                &retry,
                &replacement,
                digest(&codec::encode(&replacement, false).unwrap()),
            );
            top(&f, &retry, &retry_intent, Some("complete"));
            assert!(s.native_inventory(Some(&j), true).is_ok());
            let mut wrong = fenced;
            wrong.observed = "desired".into();
            write(&f, &fence_binding, "2049.terminal.json", &wrong);
            assert!(s.native_inventory(Some(&j), true).is_err());
        }
    }
    #[test]
    fn checked_failed_retry_chain_is_finite_and_retains_original_recipe() {
        let (f, s, j, binding) = fixture();
        let target = target();
        let original = intent(
            &binding,
            &target,
            digest(&codec::encode(&target, false).unwrap()),
        );
        top(&f, &binding, &original, Some("failed"));
        let mut retry = binding.clone();
        retry.ordinal = 2049;
        let mut next = binding.clone();
        next.ordinal = 2050;
        let first = Resolution {
            version: 1,
            binding: binding.clone(),
            intent: digest(&codec::encode(&original, true).unwrap()),
            retry: retry.clone(),
            fence: None,
        };
        write(&f, &binding, "001.recovery.json", &first);
        let second_intent = intent(&retry, &target, original.recipe.clone());
        top(&f, &retry, &second_intent, Some("failed"));
        assert!(s.native_inventory(Some(&j), true).is_err());
        let second = Resolution {
            version: 1,
            binding: retry.clone(),
            intent: digest(&codec::encode(&second_intent, true).unwrap()),
            retry: next.clone(),
            fence: None,
        };
        write(&f, &retry, "2049.recovery.json", &second);
        top(
            &f,
            &next,
            &intent(&next, &target, original.recipe.clone()),
            Some("complete"),
        );
        assert!(s.native_inventory(Some(&j), true).is_ok());
        let mut cycle = second;
        cycle.retry.ordinal = 1;
        write(&f, &retry, "2049.recovery.json", &cycle);
        assert!(s.native_inventory(Some(&j), true).is_err());
    }
}
