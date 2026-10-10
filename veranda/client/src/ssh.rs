//! Fixed SSH launch surface. No caller-controlled shell command or SSH options.
use crate::local_fleet::NativeError;
use std::io::{Read, Write};
use std::path::Path;
use std::process::{Command, Stdio};
use std::sync::mpsc;
use std::thread;
use std::time::{Duration, Instant};

const COMMAND_TIMEOUT: Duration = Duration::from_secs(8);
const MAX_CAPTURE: usize = 64 * 1024;

#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct Endpoint {
    pub destination: String,
    pub host: String,
    pub user: Option<String>,
    pub port: u16,
}

impl Endpoint {
    pub fn parse(input: &str) -> Result<Self, NativeError> {
        if input.is_empty()
            || input.len() > 320
            || input.chars().any(|c| c.is_whitespace() || c.is_control())
        {
            return Err(invalid_endpoint());
        }
        let (user, address) = match input.split_once('@') {
            Some((user, address)) if safe_user(user) && !address.contains('@') => {
                (Some(user.to_owned()), address)
            }
            Some(_) => return Err(invalid_endpoint()),
            None => (None, input),
        };
        let (host, port) = if let Some(address) = address.strip_prefix('[') {
            let (host, suffix) = address.split_once(']').ok_or_else(invalid_endpoint)?;
            if host.parse::<std::net::Ipv6Addr>().is_err() {
                return Err(invalid_endpoint());
            }
            let port = if suffix.is_empty() {
                22
            } else {
                parse_port(suffix.strip_prefix(':').ok_or_else(invalid_endpoint)?)?
            };
            (host.to_owned(), port)
        } else {
            let (host, port) = match address.split_once(':') {
                Some((host, port)) => (host, parse_port(port)?),
                None => (address, 22),
            };
            if host.is_empty()
                || host.len() > 253
                || !host
                    .bytes()
                    .all(|b| b.is_ascii_alphanumeric() || b == b'.' || b == b'-')
                || !host.as_bytes()[0].is_ascii_alphanumeric()
                || host.ends_with('.')
                || host.contains("..")
            {
                return Err(invalid_endpoint());
            }
            (host.to_ascii_lowercase(), port)
        };
        let address = if host.contains(':') {
            format!("[{host}]")
        } else {
            host.clone()
        };
        let address = if port == 22 {
            address
        } else {
            format!("{address}:{port}")
        };
        let destination = user
            .as_ref()
            .map(|user| format!("{user}@{address}"))
            .unwrap_or(address);
        Ok(Self {
            destination,
            host,
            user,
            port,
        })
    }
}

fn safe_user(user: &str) -> bool {
    !user.is_empty()
        && user.len() <= 64
        && user.as_bytes()[0].is_ascii_alphanumeric()
        && user
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"._-".contains(&b))
}
fn parse_port(port: &str) -> Result<u16, NativeError> {
    if !port.bytes().all(|b| b.is_ascii_digit()) {
        return Err(invalid_endpoint());
    }
    port.parse::<u16>()
        .ok()
        .filter(|port| *port != 0)
        .ok_or_else(invalid_endpoint)
}
fn invalid_endpoint() -> NativeError {
    NativeError::new(
        "invalid_destination",
        "Use a host or user@host, optionally followed by :port. Use brackets for IPv6.",
    )
}

#[derive(Clone)]
pub(crate) struct HostKey {
    pub key: String,
    pub fingerprint: String,
}

/// This only obtains public keys. It never authenticates or starts a trusted RPC session.
pub(crate) fn assess_host_key(endpoint: &Endpoint) -> Result<HostKey, NativeError> {
    let mut scan = Command::new("ssh-keyscan");
    scan.args([
        "-T",
        "5",
        "-p",
        &endpoint.port.to_string(),
        "-t",
        "ed25519,ecdsa,rsa",
        &endpoint.host,
    ]);
    let output = bounded_output(&mut scan, None)?;
    let output = std::str::from_utf8(&output).map_err(|_| invalid_key())?;
    // Prefer Ed25519, without depending on scan output ordering.
    for algorithm in [
        "ssh-ed25519",
        "ecdsa-sha2-nistp256",
        "ecdsa-sha2-nistp384",
        "ecdsa-sha2-nistp521",
        "ssh-rsa",
    ] {
        let mut found: Option<String> = None;
        for line in output.lines().filter(|line| !line.starts_with('#')) {
            let fields: Vec<_> = line.split_whitespace().collect();
            if fields.len() == 3 && fields[1] == algorithm {
                let key = format!("{} {}", fields[1], fields[2]);
                validate_key(&key)?;
                if found.as_ref().is_some_and(|found| found != &key) {
                    return Err(NativeError::new("ambiguous_host_key", "The endpoint returned conflicting host keys. Verify the endpoint before connecting."));
                }
                found = Some(key);
            }
        }
        if let Some(key) = found {
            return Ok(HostKey {
                fingerprint: fingerprint(&key)?,
                key,
            });
        }
    }
    Err(NativeError::new(
        "host_key_unavailable",
        "SSH did not return a supported public host key. Check the address and SSH port.",
    ))
}

pub(crate) fn validate_key(key: &str) -> Result<(), NativeError> {
    if key.len() > 8192 {
        return Err(invalid_key());
    }
    let fields: Vec<_> = key.split(' ').collect();
    if fields.len() != 2
        || ![
            "ssh-ed25519",
            "ecdsa-sha2-nistp256",
            "ecdsa-sha2-nistp384",
            "ecdsa-sha2-nistp521",
            "ssh-rsa",
        ]
        .contains(&fields[0])
        || fields[1].is_empty()
        || !fields[1]
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"+/=".contains(&b))
    {
        return Err(invalid_key());
    }
    Ok(())
}
fn fingerprint(key: &str) -> Result<String, NativeError> {
    let mut command = Command::new("ssh-keygen");
    command.args(["-l", "-E", "sha256", "-f", "-"]);
    let public_key = format!("{key}\n");
    let output = bounded_output(&mut command, Some(public_key.as_bytes()))?;
    let output = std::str::from_utf8(&output).map_err(|_| invalid_key())?;
    let value = output.split_whitespace().nth(1).ok_or_else(invalid_key)?;
    if !valid_fingerprint(value) {
        return Err(invalid_key());
    }
    Ok(value.to_owned())
}
pub(crate) fn valid_fingerprint(value: &str) -> bool {
    value.strip_prefix("SHA256:").is_some_and(|hash| {
        hash.len() == 43
            && hash
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b"+/".contains(&b))
    })
}
fn invalid_key() -> NativeError {
    NativeError::new(
        "invalid_host_key",
        "SSH returned an invalid public host key. Verify the endpoint before connecting.",
    )
}

fn pinned_command(
    endpoint: &Endpoint,
    pin_file: &Path,
    alias: &str,
    key: &str,
    tty: bool,
) -> Result<Command, NativeError> {
    validate_key(key)?;
    if alias.is_empty()
        || alias.len() > 128
        || !alias
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || b"._-".contains(&byte))
    {
        return Err(invalid_launch());
    }
    let pin_path = pin_file
        .to_str()
        .filter(|path| !path.chars().any(|c| c.is_control()))
        .ok_or_else(|| {
            NativeError::new(
                "invalid_store_path",
                "The Veranda connection store path is unsupported.",
            )
        })?;
    let algorithm = key.split(' ').next().ok_or_else(invalid_key)?;
    let algorithms = if algorithm == "ssh-rsa" {
        "rsa-sha2-512,rsa-sha2-256"
    } else {
        algorithm
    };
    let mut command = Command::new("ssh");
    command.args(["-F", "none"]);
    command.arg(if tty { "-tt" } else { "-T" });
    command.args([
        "-o",
        "BatchMode=yes",
        "-o",
        "StrictHostKeyChecking=yes",
        "-o",
        "GlobalKnownHostsFile=none",
        "-o",
        "UpdateHostKeys=no",
        "-o",
        "VerifyHostKeyDNS=no",
        "-o",
        "PasswordAuthentication=no",
        "-o",
        "KbdInteractiveAuthentication=no",
        "-o",
        "PreferredAuthentications=publickey",
        "-o",
        "IdentityFile=none",
        "-o",
        "IdentitiesOnly=no",
        "-o",
        "ForwardAgent=no",
        "-o",
        "ConnectionAttempts=1",
        "-o",
        "ConnectTimeout=5",
        "-o",
        "ServerAliveInterval=5",
        "-o",
        "ServerAliveCountMax=1",
    ]);
    command.arg("-o").arg(format!(
        "UserKnownHostsFile=\"{}\"",
        pin_path
            .replace('\\', "\\\\")
            .replace('"', "\\\"")
            .replace('%', "%%")
    ));
    command.arg("-o").arg(format!("HostKeyAlias={alias}"));
    command
        .arg("-o")
        .arg(format!("HostKeyAlgorithms={algorithms}"));
    command.args(["-p", &endpoint.port.to_string()]);
    if let Some(user) = &endpoint.user {
        command.args(["-l", user]);
    }
    command.env("SSH_ASKPASS_REQUIRE", "never");
    Ok(command)
}

pub(crate) fn rpc_command(
    endpoint: &Endpoint,
    pin_file: &Path,
    alias: &str,
    key: &str,
    yard: Option<&str>,
) -> Result<Command, NativeError> {
    let mut command = pinned_command(endpoint, pin_file, alias, key, false)?;
    let remote_command = if let Some(yard) = yard {
        if !crate::local_fleet::safe_name(yard) {
            return Err(invalid_launch());
        }
        format!("yard -Y {yard} rpc --stdio")
    } else {
        "yard rpc --stdio".into()
    };
    command.args(["--", &endpoint.host, &remote_command]);
    Ok(command)
}

pub(crate) fn owner_command(
    endpoint: &Endpoint,
    pin_file: &Path,
    alias: &str,
    key: &str,
    arguments: &[String],
    tty: bool,
) -> Result<Command, NativeError> {
    if !valid_owner_arguments(arguments) {
        return Err(invalid_launch());
    }
    let mut command = pinned_command(endpoint, pin_file, alias, key, tty)?;
    let remote_command = arguments
        .iter()
        .map(|argument| shell_quote(argument))
        .collect::<Vec<_>>()
        .join(" ");
    command.args(["--", &endpoint.host, &remote_command]);
    Ok(command)
}

pub(crate) fn proxy_command(
    endpoint: &Endpoint,
    pin_file: &Path,
    alias: &str,
    key: &str,
    port: u16,
) -> Result<Command, NativeError> {
    if port == 0 {
        return Err(invalid_launch());
    }
    let mut command = pinned_command(endpoint, pin_file, alias, key, false)?;
    command.arg("-W").arg(format!("127.0.0.1:{port}"));
    command.args(["--", &endpoint.host]);
    Ok(command)
}

fn valid_owner_arguments(arguments: &[String]) -> bool {
    match arguments {
        [command, flag] if command == "bash" && flag == "-l" => true,
        [command] if command == "htop" => true,
        [command, selector, yard, subcommand, rest @ ..]
            if command == "yard"
                && selector == "-Y"
                && crate::local_fleet::safe_name(yard)
                && subcommand == "shell" =>
        {
            let project = match rest {
                [separator, command] if separator == "--" && command == "htop" => &[][..],
                [project, separator, command] if separator == "--" && command == "htop" => {
                    std::slice::from_ref(project)
                }
                [] | [_] => rest,
                _ => return false,
            };
            project.iter().all(|value| {
                !value.is_empty()
                    && value.len() <= 128
                    && value.as_bytes()[0].is_ascii_alphanumeric()
                    && value
                        .bytes()
                        .all(|byte| byte.is_ascii_alphanumeric() || b"._-".contains(&byte))
            })
        }
        _ => false,
    }
}

fn shell_quote(value: &str) -> String {
    format!("'{}'", value.replace('\'', "'\\''"))
}
fn invalid_launch() -> NativeError {
    NativeError::new(
        "invalid_native_launch",
        "Select a supported owner or yard action before opening the native tool.",
    )
}

/// Public command output only; all errors redact subprocess output and file paths.
pub(crate) fn bounded_output(
    command: &mut Command,
    input: Option<&[u8]>,
) -> Result<Vec<u8>, NativeError> {
    capture_with_timeout(command, input, COMMAND_TIMEOUT)
}

#[cfg(test)]
pub(crate) fn bounded_output_until(
    command: &mut Command,
    input: Option<&[u8]>,
    deadline: Instant,
) -> Result<Vec<u8>, NativeError> {
    let remaining = deadline
        .checked_duration_since(Instant::now())
        .filter(|remaining| !remaining.is_zero())
        .ok_or_else(|| {
            NativeError::new(
                "ssh_timeout",
                "The SSH tool did not finish in time. Try again.",
            )
        })?;
    capture_with_timeout(command, input, remaining.min(COMMAND_TIMEOUT))
}

/// Credential material stays in the system agent. Only readiness crosses the
/// native boundary; the public fingerprint listing is discarded immediately.
pub(crate) fn require_agent() -> Result<(), NativeError> {
    let mut command = Command::new("ssh-add");
    command.arg("-l");
    agent_readiness(&mut command)
}
fn agent_readiness(command: &mut Command) -> Result<(), NativeError> {
    bounded_output(command, None).map(|_| ()).map_err(|_| {
        NativeError::new(
            "ssh_agent_unavailable",
            "Start or unlock the system SSH agent and load an authorized key, then reconnect.",
        )
    })
}

fn capture_with_timeout(
    command: &mut Command,
    input: Option<&[u8]>,
    timeout: Duration,
) -> Result<Vec<u8>, NativeError> {
    command
        .stdin(if input.is_some() {
            Stdio::piped()
        } else {
            Stdio::null()
        })
        .stdout(Stdio::piped())
        .stderr(Stdio::null());
    #[cfg(unix)]
    {
        use std::os::unix::process::CommandExt;
        command.process_group(0);
    }
    let mut child = command.spawn().map_err(|_| {
        NativeError::new(
            "ssh_tool_unavailable",
            "Install OpenSSH client tools (ssh, ssh-keyscan and ssh-keygen) and try again.",
        )
    })?;
    let stdout = child.stdout.take().ok_or_else(|| {
        NativeError::new("ssh_io_failed", "Could not read the SSH tool response.")
    })?;
    let (sender, receiver) = mpsc::sync_channel(1);
    let reader = thread::spawn(move || {
        let mut bytes = Vec::new();
        let result = stdout
            .take(MAX_CAPTURE as u64 + 1)
            .read_to_end(&mut bytes)
            .map(|_| bytes);
        let _ = sender.send(result);
    });
    let writer = if let (Some(mut stdin), Some(input)) = (child.stdin.take(), input) {
        let input = input.to_vec();
        Some(thread::spawn(move || {
            let _ = stdin.write_all(&input);
        }))
    } else {
        None
    };
    let deadline = Instant::now() + timeout;
    let mut captured = None;
    let mut exited = false;
    let mut error = None;
    loop {
        if captured.is_none() {
            match receiver.try_recv() {
                Ok(Ok(bytes)) if bytes.len() <= MAX_CAPTURE => captured = Some(bytes),
                Ok(Ok(_)) => {
                    error = Some(NativeError::new(
                        "ssh_output_limit",
                        "The SSH tool returned too much data.",
                    ));
                    break;
                }
                Ok(Err(_)) => {
                    error = Some(NativeError::new(
                        "ssh_io_failed",
                        "Could not read the SSH tool response.",
                    ));
                    break;
                }
                Err(mpsc::TryRecvError::Disconnected) => {
                    error = Some(NativeError::new(
                        "ssh_io_failed",
                        "Could not read the SSH tool response.",
                    ));
                    break;
                }
                Err(mpsc::TryRecvError::Empty) => {}
            }
        }
        if capture_child_exited(&mut child) {
            exited = true;
            // Revoke descendants before waiting for inherited output pipes. The
            // leader is not reaped until the owned group is killed below.
            break;
        }
        if Instant::now() >= deadline {
            error = Some(NativeError::new(
                "ssh_timeout",
                "The SSH tool did not finish in time. Try again.",
            ));
            break;
        }
        thread::sleep(Duration::from_millis(10));
    }
    #[cfg(unix)]
    unsafe {
        libc::kill(-(child.id() as i32), libc::SIGKILL);
    }
    let _ = child.kill();
    let status = child.wait();
    if captured.is_none() && error.is_none() {
        captured = match receiver.recv_timeout(Duration::from_millis(100)) {
            Ok(Ok(bytes)) if bytes.len() <= MAX_CAPTURE => Some(bytes),
            Ok(Ok(_)) => {
                error = Some(NativeError::new(
                    "ssh_output_limit",
                    "The SSH tool returned too much data.",
                ));
                None
            }
            _ => {
                error = Some(NativeError::new(
                    "ssh_io_failed",
                    "Could not read the SSH tool response.",
                ));
                None
            }
        };
    }
    let result = if let Some(error) = error {
        Err(error)
    } else if exited && status.is_ok_and(|status| status.success()) {
        captured.ok_or_else(|| {
            NativeError::new("ssh_io_failed", "Could not read the SSH tool response.")
        })
    } else {
        Err(NativeError::new("ssh_probe_failed", "SSH could not obtain or validate the host key. Check the address, SSH port and network."))
    };
    if let Some(writer) = writer {
        let _ = writer.join();
    }
    let _ = reader.join();
    result
}

fn capture_child_exited(child: &mut std::process::Child) -> bool {
    #[cfg(unix)]
    {
        let mut status: libc::siginfo_t = unsafe { std::mem::zeroed() };
        let result = unsafe {
            libc::waitid(
                libc::P_PID,
                child.id(),
                &mut status,
                libc::WEXITED | libc::WNOHANG | libc::WNOWAIT,
            )
        };
        result != 0 || unsafe { status.si_pid() } != 0
    }
    #[cfg(not(unix))]
    {
        matches!(child.try_wait(), Ok(Some(_)))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn owner_shell_launches_only_fixed_patterns_and_quotes_each_argument() {
        assert_eq!(shell_quote("a'b;$(x)"), "'a'\\''b;$(x)'");
        let endpoint = Endpoint::parse("user@host").unwrap();
        let arguments =
            ["yard", "-Y", "default", "shell", "project-1", "--", "htop"].map(str::to_owned);
        let command = owner_command(
            &endpoint,
            Path::new("/synthetic"),
            "alias",
            "ssh-ed25519 AAAA",
            &arguments,
            true,
        )
        .unwrap();
        let args: Vec<_> = command
            .get_args()
            .map(|value| value.to_string_lossy().into_owned())
            .collect();
        assert!(args.contains(&"-tt".into()));
        assert_eq!(
            args.last().unwrap(),
            "'yard' '-Y' 'default' 'shell' 'project-1' '--' 'htop'"
        );
        for invalid in [
            vec!["ssh", "-i", "private.pem"],
            vec!["bash", "-c", "anything"],
            vec!["yard", "-Y", "default", "shell", "--", "anything"],
            vec!["yard", "-Y", "default", "shell", "project;bad"],
        ] {
            assert!(owner_command(
                &endpoint,
                Path::new("/synthetic"),
                "alias",
                "ssh-ed25519 AAAA",
                &invalid.into_iter().map(str::to_owned).collect::<Vec<_>>(),
                true
            )
            .is_err());
        }
    }
    #[test]
    fn native_proxy_targets_only_the_typed_owner_loopback_port() {
        let endpoint = Endpoint::parse("user@host").unwrap();
        let command = proxy_command(
            &endpoint,
            Path::new("/synthetic"),
            "alias",
            "ssh-ed25519 AAAA",
            2222,
        )
        .unwrap();
        let args: Vec<_> = command
            .get_args()
            .map(|value| value.to_string_lossy().into_owned())
            .collect();
        assert!(args
            .windows(2)
            .any(|values| values == ["-W", "127.0.0.1:2222"]));
        assert_eq!(&args[args.len() - 2..], ["--", "host"]);
        assert!(args.contains(&"IdentityFile=none".into()));
        assert!(proxy_command(
            &endpoint,
            Path::new("/synthetic"),
            "alias",
            "ssh-ed25519 AAAA",
            0
        )
        .is_err());
    }
    #[test]
    fn endpoint_options_and_shell_metacharacters_are_rejected() {
        for input in [
            "",
            "-oProxyCommand=bad",
            "user@host;bad",
            "user@host bad",
            "user@host\n",
            "user@@host",
            "user@host:0",
            "host:65536",
            "host:22:33",
            "@host",
            "user@/tmp/key",
            "host$(id)",
        ] {
            assert!(Endpoint::parse(input).is_err(), "{input:?}");
        }
        assert_eq!(
            Endpoint::parse("user@EXAMPLE.test:2222")
                .unwrap()
                .destination,
            "user@example.test:2222"
        );
        assert_eq!(
            Endpoint::parse("user@[::1]:22").unwrap().destination,
            "user@[::1]"
        );
    }
    #[test]
    #[cfg(unix)]
    fn agent_readiness_discards_output_and_exposes_only_an_actionable_static_error() {
        let mut ready = Command::new("sh");
        ready.args(["-c", "printf 'synthetic public credential reference'"]);
        assert!(agent_readiness(&mut ready).is_ok());
        let mut absent = Command::new("sh");
        absent.args(["-c", "printf 'synthetic private agent detail' >&2; exit 2"]);
        let error = agent_readiness(&mut absent).unwrap_err();
        assert_eq!(error.code, "ssh_agent_unavailable");
        assert!(!error.message.contains("synthetic"));
        assert!(error.message.contains("system SSH agent"));
    }
    #[test]
    fn percent_tokens_in_app_owned_pin_paths_are_literal() {
        let endpoint = Endpoint::parse("user@host").unwrap();
        let command = rpc_command(
            &endpoint,
            Path::new("/synthetic%h/pin%file"),
            "alias",
            "ssh-ed25519 AAAA",
            None,
        )
        .unwrap();
        assert!(command
            .get_args()
            .any(|argument| argument == "UserKnownHostsFile=\"/synthetic%%h/pin%%file\""));
    }
    #[test]
    fn launch_is_fixed_and_strictly_pinned() {
        let endpoint = Endpoint::parse("user@host:2222").unwrap();
        let command = rpc_command(
            &endpoint,
            Path::new("/synthetic pin"),
            "veranda-token",
            "ssh-ed25519 AAAA",
            None,
        )
        .unwrap();
        let args: Vec<_> = command
            .get_args()
            .map(|arg| arg.to_string_lossy().into_owned())
            .collect();
        assert!(args.contains(&"StrictHostKeyChecking=yes".into()));
        assert!(args.contains(&"GlobalKnownHostsFile=none".into()));
        assert!(args.contains(&"UserKnownHostsFile=\"/synthetic pin\"".into()));
        assert_eq!(&args[args.len() - 3..], ["--", "host", "yard rpc --stdio"]);
    }
    #[test]
    fn fingerprint_is_computed_by_openssh_from_public_key_only() {
        let key =
            "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFNjq/M43XXNjEd85hzUhKYHHWFifIjMPICWzp++Ciy4";
        assert_eq!(
            fingerprint(key).unwrap(),
            "SHA256:TxZK+ksH4eweEfOgYqfGycNVLmMqGVADtutL+em5rYg"
        );
        assert!(fingerprint("ssh-ed25519 AAAA").is_err());
    }
    #[cfg(unix)]
    #[test]
    fn capture_revokes_inherited_pipes_even_when_the_tool_leader_exits_first() {
        let mut command = Command::new("sh");
        command.args(["-c", "sleep 60 & printf 'synthetic complete public output'"]);
        let started = Instant::now();
        let bytes = capture_with_timeout(&mut command, None, Duration::from_secs(2)).unwrap();
        assert_eq!(bytes, b"synthetic complete public output");
        assert!(started.elapsed() < Duration::from_secs(1));
    }
    #[cfg(unix)]
    #[test]
    fn timeout_kills_and_reaps_the_child_without_waiting_for_natural_exit() {
        let mut command = Command::new("sleep");
        command.arg("2");
        let started = Instant::now();
        assert_eq!(
            capture_with_timeout(&mut command, None, Duration::from_millis(30))
                .unwrap_err()
                .code,
            "ssh_timeout"
        );
        assert!(started.elapsed() < Duration::from_secs(1));
    }
    #[cfg(unix)]
    #[test]
    fn bounded_deadline_refuses_expired_work_and_preserves_capture_errors() {
        let mut random = [0; 16];
        getrandom::fill(&mut random).unwrap();
        let marker = std::env::temp_dir().join(format!(
            "veranda-deadline-{:032x}",
            u128::from_ne_bytes(random)
        ));
        assert!(!marker.exists());
        let mut mutation = Command::new("touch");
        mutation.arg(&marker);
        assert_eq!(
            bounded_output_until(&mut mutation, None, Instant::now())
                .unwrap_err()
                .code,
            "ssh_timeout"
        );
        assert!(!marker.exists(), "expired command was spawned");

        let mut delayed = Command::new("sleep");
        delayed.arg("2");
        let started = Instant::now();
        assert_eq!(
            bounded_output_until(&mut delayed, None, started + Duration::from_millis(30))
                .unwrap_err()
                .code,
            "ssh_timeout"
        );
        assert!(started.elapsed() < Duration::from_secs(1));
        let deadline = Instant::now() + Duration::from_secs(2);
        let mut ready = Command::new("sh");
        ready.args(["-c", "printf ready"]);
        assert_eq!(
            bounded_output_until(&mut ready, None, deadline).unwrap(),
            b"ready"
        );
        let mut failed = Command::new("sh");
        failed.args(["-c", "printf 'synthetic private error' >&2; exit 2"]);
        let error = bounded_output_until(&mut failed, None, deadline).unwrap_err();
        assert_eq!(error.code, "ssh_probe_failed");
        assert!(!error.message.contains("synthetic"));
    }
    #[cfg(unix)]
    #[test]
    fn output_capture_is_bounded() {
        let mut command = Command::new("head");
        command.args(["-c", "70000", "/dev/zero"]);
        assert_eq!(
            bounded_output(&mut command, None).unwrap_err().code,
            "ssh_output_limit"
        );
    }
}
