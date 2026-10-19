//! Inert private operation entrypoint. Delivery has not selected this owner yet.
use serde::{Deserialize, Serialize};
use std::{
    fs::File,
    io::{self, BufRead, BufReader, Write},
    os::unix::net::UnixStream,
    time::{Duration, Instant},
};
use subyard_release_owner::{
    owner::{Grant, Options, Owner},
    wire::process_v1 as p,
    Error, Result,
};
#[derive(Deserialize)]
#[serde(tag = "op", rename_all = "kebab-case", deny_unknown_fields)]
enum Request {
    Open {
        options: Box<Options>,
    },
    Inspect {
        goal: p::Goal,
    },
    InspectRecovery {
        replacement: subyard_release_owner::wire::recovery_v1::ActivationOnlyRecoveryRequest,
        contract: String,
    },
    InspectMigrationOwner {
        replacement:
            subyard_release_owner::wire::migration_owner_v1::MigrationOwnerReplacementRequest,
    },
    Execute {
        plan: String,
        authorization: String,
    },
    Resume,
    Capabilities {
        kind: String,
    },
}
#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct Reply {
    #[serde(skip_serializing_if = "Option::is_none")]
    retained_releases: Option<Vec<String>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    admission: Option<subyard_release_owner::migration_operation::ResumeAdmission>,
    activation_reconciliation_owned: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    capabilities: Option<subyard_release_owner::wire::process_v2::RecoveryCapabilities>,
    #[serde(skip_serializing_if = "Option::is_none")]
    activation_owners: Option<Vec<String>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    custody: Option<subyard_release_owner::owner::CustodyContext>,
    #[serde(skip_serializing_if = "Option::is_none")]
    inspection: Option<p::Inspection>,
    #[serde(skip_serializing_if = "Option::is_none")]
    outcome: Option<p::Outcome>,
    #[serde(skip_serializing_if = "Option::is_none")]
    error: Option<String>,
}
fn read(input: &mut BufReader<UnixStream>, initial: bool) -> Result<Option<Request>> {
    let mut raw = Vec::new();
    let mut frame_end: Option<Instant> = None;
    loop {
        let timeout = match frame_end {
            Some(end) => end
                .checked_duration_since(Instant::now())
                .ok_or(Error::Io)?,
            None => Duration::from_secs(if initial { 5 } else { 300 }),
        };
        input
            .get_ref()
            .set_read_timeout(Some(timeout))
            .map_err(|_| Error::Io)?;
        let bytes = input.fill_buf().map_err(|_| Error::Io)?;
        if bytes.is_empty() {
            return if raw.is_empty() {
                Ok(None)
            } else {
                Err(Error::Codec)
            };
        }
        if frame_end.is_none() {
            frame_end = Some(Instant::now() + Duration::from_secs(5))
        }
        let newline = bytes.iter().position(|b| *b == b'\n');
        let count = newline.map_or(bytes.len(), |n| n + 1);
        if raw.len().checked_add(count).ok_or(Error::Bound)? > 8 << 20 {
            return Err(Error::Bound);
        };
        raw.extend_from_slice(&bytes[..count]);
        input.consume(count);
        if newline.is_some() {
            return serde_json::from_slice(&raw)
                .map(Some)
                .map_err(|_| Error::Codec);
        }
    }
}
fn send(reply: Reply, output: &mut UnixStream) -> Result<()> {
    let raw = subyard_release_owner::codec::encode(&reply, true)?;
    let end = Instant::now() + Duration::from_secs(5);
    let mut offset = 0;
    while offset < raw.len() {
        output
            .set_write_timeout(Some(
                end.checked_duration_since(Instant::now())
                    .ok_or(Error::Io)?,
            ))
            .map_err(|_| Error::Io)?;
        let n = output.write(&raw[offset..]).map_err(|_| Error::Io)?;
        if n == 0 {
            return Err(Error::Io);
        };
        offset += n;
    }
    Ok(())
}
fn capabilities() -> Result<(File, File, File, File)> {
    use rustix::net::{RecvAncillaryBuffer, RecvAncillaryMessage, RecvFlags, ReturnFlags};
    let mut byte = [0];
    let mut space = [std::mem::MaybeUninit::uninit(); rustix::cmsg_space!(ScmRights(5))];
    let mut ancillary = RecvAncillaryBuffer::new(&mut space);
    let stdin = io::stdin();
    let mut ready = [rustix::event::PollFd::new(
        &stdin,
        rustix::event::PollFlags::IN,
    )];
    let timeout = rustix::event::Timespec {
        tv_sec: 5,
        tv_nsec: 0,
    };
    rustix::event::poll(&mut ready, Some(&timeout)).map_err(|_| Error::Io)?;
    if ready[0].revents().is_empty() {
        return Err(Error::Io);
    };
    let message = rustix::net::recvmsg(
        io::stdin(),
        &mut [io::IoSliceMut::new(&mut byte)],
        &mut ancillary,
        RecvFlags::CMSG_CLOEXEC,
    )
    .map_err(|_| Error::Io)?;
    let mut files = vec![];
    let mut unexpected = false;
    for message in ancillary.drain() {
        match message {
            RecvAncillaryMessage::ScmRights(fds) => files.extend(fds),
            _ => unexpected = true,
        }
    }
    if unexpected
        || message.bytes != 1
        || byte != *b"C"
        || message
            .flags
            .intersects(ReturnFlags::CTRUNC | ReturnFlags::TRUNC)
        || !(files.len() == 4 || cfg!(feature = "fixture-cuts") && files.len() == 5)
    {
        return Err(Error::Invalid);
    }
    #[cfg(feature = "fixture-cuts")]
    if files.len() == 5 {
        let control = File::from(files.pop().ok_or(Error::Invalid)?);
        subyard_release_owner::fixture::install(control)?;
    }
    let repository = File::from(files.pop().ok_or(Error::Invalid)?);
    let custody = File::from(files.pop().ok_or(Error::Invalid)?);
    let grant = File::from(files.pop().ok_or(Error::Invalid)?);
    let helper = File::from(files.pop().ok_or(Error::Invalid)?);
    Ok((helper, grant, custody, repository))
}
fn run() -> Result<()> {
    if std::env::args().collect::<Vec<_>>()
        != [
            std::env::args().next().ok_or(Error::Invalid)?,
            "_release-owner-operation-v1".into(),
        ]
    {
        return Err(Error::Invalid);
    }
    let (helper, grant_input, custody_ingress, repository) = capabilities()?;
    let descriptor = rustix::io::dup(io::stdin()).map_err(|_| Error::Io)?;
    rustix::io::fcntl_setfd(&descriptor, rustix::io::FdFlags::CLOEXEC).map_err(|_| Error::Io)?;
    let socket = UnixStream::from(descriptor);
    let mut input = BufReader::new(socket);
    let Some(Request::Open { options }) = read(&mut input, true)? else {
        return Err(Error::Invalid);
    };
    // Received roles are CLOEXEC-owned values; no raw inherited grant FD can
    // leak into the retained helper. The delivery facade authenticates the image.
    let owner = Owner::open(*options, helper, custody_ingress, repository)?;
    let mut grant = Some(Grant::capture(grant_input)?);
    let mut prepared = None;
    let mut recovery_prepared = None;
    let mut migration_prepared = None;
    send(
        Reply {
            retained_releases: None,
            admission: None,
            activation_reconciliation_owned: true,
            capabilities: None,
            activation_owners: None,
            custody: Some(owner.custody_context()),
            inspection: None,
            outcome: None,
            error: None,
        },
        input.get_mut(),
    )?;
    while let Some(request) = read(&mut input, false)? {
        let result = match request {
            Request::Inspect { goal } => {
                if prepared.is_some() || recovery_prepared.is_some() || migration_prepared.is_some()
                {
                    Err(Error::Invalid)
                } else {
                    owner
                        .prepare(goal, grant.take().ok_or(Error::Invalid)?)
                        .map(|value| {
                            let admission = value.resume_admission();
                            let inspection = value.inspection();
                            prepared = Some(value);
                            Reply {
                                retained_releases: None,
                                admission,
                                activation_reconciliation_owned: true,
                                capabilities: None,
                                activation_owners: None,
                                custody: None,
                                inspection: Some(inspection),
                                outcome: None,
                                error: None,
                            }
                        })
                }
            }
            Request::InspectRecovery {
                replacement,
                contract,
            } => {
                if prepared.is_some() || recovery_prepared.is_some() || migration_prepared.is_some()
                {
                    Err(Error::Invalid)
                } else {
                    owner
                        .prepare_recovery(
                            replacement,
                            &contract,
                            grant.take().ok_or(Error::Invalid)?,
                        )
                        .map(|value| {
                            let inspection = value.inspection();
                            recovery_prepared = Some(value);
                            Reply {
                                retained_releases: None,
                                admission: None,
                                activation_reconciliation_owned: true,
                                capabilities: None,
                                activation_owners: None,
                                custody: None,
                                inspection: Some(inspection),
                                outcome: None,
                                error: None,
                            }
                        })
                }
            }
            Request::InspectMigrationOwner { replacement } => {
                if prepared.is_some() || recovery_prepared.is_some() || migration_prepared.is_some()
                {
                    Err(Error::Invalid)
                } else {
                    owner
                        .prepare_migration_owner(replacement, grant.take().ok_or(Error::Invalid)?)
                        .map(|value| {
                            let admission = value.resume_admission();
                            let inspection = value.inspection();
                            migration_prepared = Some(value);
                            Reply {
                                retained_releases: None,
                                admission,
                                activation_reconciliation_owned: true,
                                capabilities: None,
                                activation_owners: None,
                                custody: None,
                                inspection: Some(inspection),
                                outcome: None,
                                error: None,
                            }
                        })
                }
            }
            Request::Execute {
                plan,
                authorization,
            } => if let Some(value) = migration_prepared.take() {
                value
                    .confirm(&plan, &authorization)
                    .and_then(|v| v.execute())
            } else if let Some(value) = recovery_prepared.take() {
                value
                    .confirm(&plan, &authorization)
                    .and_then(|v| v.execute())
            } else {
                prepared
                    .take()
                    .ok_or(Error::Invalid)
                    .and_then(|value| value.confirm(&plan, &authorization))
                    .and_then(|value| value.execute())
            }
            .map(|outcome| Reply {
                retained_releases: None,
                admission: None,
                activation_reconciliation_owned: true,
                capabilities: None,
                activation_owners: None,
                custody: None,
                inspection: None,
                outcome: Some(outcome),
                error: None,
            }),
            Request::Resume => {
                if prepared
                    .as_ref()
                    .is_some_and(|p| p.resume_admission().is_some())
                {
                    drop(prepared.take());
                }
                drop(migration_prepared.take());
                if prepared.is_some() || recovery_prepared.is_some() {
                    Err(Error::Invalid)
                } else {
                    owner.resume().map(|outcome| Reply {
                        retained_releases: None,
                        admission: None,
                        activation_reconciliation_owned: true,
                        capabilities: None,
                        activation_owners: None,
                        custody: None,
                        inspection: None,
                        outcome: Some(outcome),
                        error: None,
                    })
                }
            }
            Request::Capabilities { kind } => owner.process_capabilities(&kind).map(|r| Reply {
                retained_releases: None,
                admission: None,
                activation_reconciliation_owned: r.activation_reconciliation_owned,
                capabilities: r.capabilities,
                activation_owners: r.activation_owners,
                custody: None,
                inspection: None,
                outcome: None,
                error: None,
            }),
            Request::Open { .. } => Err(Error::Invalid),
        };
        match result {
            Ok(mut reply) => {
                if reply.inspection.is_some() || reply.outcome.is_some() {
                    reply.retained_releases = Some(owner.retained_releases()?);
                }
                send(reply, input.get_mut())?;
            }
            Err(error) => {
                send(
                    Reply {
                        retained_releases: None,
                        admission: None,
                        activation_reconciliation_owned: true,
                        capabilities: None,
                        activation_owners: None,
                        custody: None,
                        inspection: None,
                        outcome: None,
                        error: Some(format!("{error:?}").to_ascii_lowercase()),
                    },
                    input.get_mut(),
                )?;
                return Err(error);
            }
        }
    }
    Ok(())
}
fn main() {
    if let Err(error) = run() {
        eprintln!("release owner operation refused: {error:?}");
        std::process::exit(1)
    }
}
