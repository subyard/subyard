//! Typed owner launch descriptors become fixed native invocations, never frontend paths.
use crate::connections::{ConnectionStore, ConsentedConnection};
use crate::local_fleet::{safe_id, safe_name, NativeError};
use crate::{ssh, transport};
use serde::{Deserialize, Serialize};
use std::path::Path;
use std::process::{Command, Stdio};
use std::sync::{
    atomic::{AtomicUsize, Ordering},
    Arc,
};
use std::thread;

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Descriptor {
    schema_version: u32,
    kind: String,
    scope: String,
    yard_name: String,
    project_id: Option<String>,
    owner_arguments: Option<Vec<String>>,
    local_arguments: Option<Vec<String>>,
    vscode: Option<CodeTarget>,
}
#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct CodeTarget {
    ssh_alias: String,
    dev_user: String,
    address: String,
    port: u16,
    folder_path: String,
    host_key: String,
    host_key_fingerprint: String,
    remote_authentication: String,
}
impl Descriptor {
    pub fn validate(
        &self,
        kind: &str,
        yard: &str,
        host_scope: bool,
        project: Option<&str>,
    ) -> Result<(), NativeError> {
        if self.schema_version != 1
            || self.kind != kind
            || self.yard_name != yard
            || self.scope != if host_scope { "host" } else { "yard" }
            || self.project_id.as_deref() != project
            || !safe_name(yard)
            || project.is_some_and(|id| !safe_id(id, 128))
        {
            return Err(transport::invalid());
        }
        let mut expected = if host_scope {
            if kind == "resources" {
                vec!["htop".into()]
            } else {
                vec!["bash".into(), "-l".into()]
            }
        } else {
            vec![
                "yard".into(),
                "-Y".into(),
                yard.into(),
                if kind == "vscode" {
                    "code".into()
                } else {
                    "shell".into()
                },
            ]
        };
        if let Some(project) = project {
            expected.push(project.into());
        }
        if !host_scope && kind == "resources" {
            expected.extend(["--".into(), "htop".into()]);
        }
        if kind == "vscode" {
            let target = self.vscode.as_ref().ok_or_else(transport::invalid)?;
            if host_scope
                || self.local_arguments.as_ref() != Some(&expected)
                || self.owner_arguments.is_some()
                || !transport::identity(&target.ssh_alias)
                || !safe_name(&target.dev_user)
                || target.address != "127.0.0.1"
                || target.port == 0
                || target.remote_authentication != "already-authorized-desktop-agent-key"
                || !target.folder_path.starts_with('/')
                || target.folder_path.len() > 4096
                || target
                    .folder_path
                    .chars()
                    .any(|character| character.is_control())
                || !target.host_key.starts_with("ssh-ed25519 ")
                || !ssh::valid_fingerprint(&target.host_key_fingerprint)
            {
                return Err(transport::invalid());
            }
            ssh::validate_key(&target.host_key)?;
        } else if !matches!(kind, "shell" | "resources")
            || self.owner_arguments.as_ref() != Some(&expected)
            || self.local_arguments.is_some()
            || self.vscode.is_some()
        {
            return Err(transport::invalid());
        }
        Ok(())
    }
}
#[derive(Serialize)]
pub struct Launch {
    command: String,
}

fn owner_command(
    descriptor: &Descriptor,
    pin: Option<&ConsentedConnection>,
) -> Result<Command, NativeError> {
    let arguments = descriptor
        .owner_arguments
        .as_deref()
        .ok_or_else(transport::invalid)?;
    if let Some(pin) = pin {
        pin.owner_command(arguments, true)
    } else {
        from_arguments(arguments)
    }
}

pub fn shell_command(
    descriptor: Descriptor,
    pin: Option<ConsentedConnection>,
) -> Result<Launch, NativeError> {
    if descriptor.kind != "shell" {
        return Err(transport::invalid());
    }
    Ok(Launch {
        command: display_command(&owner_command(&descriptor, pin.as_ref())?)?,
    })
}

pub fn launch(
    descriptor: Descriptor,
    pin: Option<ConsentedConnection>,
    store: &mut ConnectionStore,
    count: Arc<AtomicUsize>,
    editor_id: &str,
) -> Result<Launch, NativeError> {
    if count.fetch_add(1, Ordering::AcqRel) >= 16 {
        count.fetch_sub(1, Ordering::AcqRel);
        return Err(NativeError::new(
            "session_capacity",
            "Close an existing Veranda terminal or editor window before opening another.",
        ));
    }
    let result = (|| {
        let mut command = if descriptor.kind == "vscode" {
            if let Some(pin) = &pin {
                require_remote_ssh()?;
                let target = descriptor.vscode.as_ref().ok_or_else(transport::invalid)?;
                let owner_pin = pin.public_pin();
                let guest_pin = format!("veranda-guest-{editor_id} {}\n", target.host_key);
                let root = store.prepare_editor_profile(editor_id)?;
                store.write_editor_profile(
                    &root,
                    &[
                        ("owner-known-hosts", &owner_pin),
                        ("guest-known-hosts", &guest_pin),
                    ],
                )?;
                let proxy =
                    pin.proxy_command_with_pin(target.port, &root.join("owner-known-hosts"))?;
                let config = format!("Host veranda-{editor_id}\n HostName 127.0.0.1\n User {}\n Port {}\n HostKeyAlias veranda-guest-{editor_id}\n UserKnownHostsFile {}\n GlobalKnownHostsFile none\n StrictHostKeyChecking yes\n UpdateHostKeys no\n VerifyHostKeyDNS no\n BatchMode yes\n IdentityFile none\n IdentitiesOnly no\n ForwardAgent no\n ProxyCommand {}\n", target.dev_user, target.port,
                    ssh_config_path(&root.join("guest-known-hosts"))?, display_command(&proxy)?.replace('%', "%%"));
                // VS Code's dedicated app-local user data keeps global SSH/Code settings untouched.
                let settings = serde_json::to_string(&serde_json::json!({"remote.SSH.configFile":root.join("ssh-config"),"remote.SSH.showLoginTerminal":false})).map_err(|_| transport::invalid())?;
                store.write_editor_profile(
                    &root,
                    &[
                        ("ssh-config", &config),
                        ("userdata/User/settings.json", &settings),
                    ],
                )?;
                let mut command = Command::new(code_program());
                command
                    .args(["--wait", "--new-window", "--user-data-dir"])
                    .arg(root.join("userdata"))
                    .args(["--remote", &format!("ssh-remote+veranda-{editor_id}")])
                    .arg(&target.folder_path);
                command
            } else {
                from_arguments(
                    descriptor
                        .local_arguments
                        .as_deref()
                        .ok_or_else(transport::invalid)?,
                )?
            }
        } else {
            owner_command(&descriptor, pin.as_ref())?
        };
        let display = display_command(&command)?;
        if descriptor.kind != "vscode" {
            command = terminal(command)?;
        }
        // A Windows SSH child owns its new console. Explicit NUL handles would
        // override its keyboard and screen and immediately end an interactive shell.
        if !cfg!(windows) || descriptor.kind == "vscode" {
            command
                .stdin(Stdio::null())
                .stdout(Stdio::null())
                .stderr(Stdio::null());
        }
        let mut child = command.spawn().map_err(|_| NativeError::new(
            "session_tool_missing", "Install a terminal or VS Code with Remote SSH on this client. For CPU / RAM, install htop on the selected owner or open its shell."))?;
        let count = count.clone();
        thread::spawn(move || {
            let _ = child.wait();
            drop(pin);
            count.fetch_sub(1, Ordering::AcqRel);
        });
        Ok(Launch { command: display })
    })();
    if result.is_err() {
        count.fetch_sub(1, Ordering::AcqRel);
    }
    result
}
fn from_arguments(arguments: &[String]) -> Result<Command, NativeError> {
    let (program, arguments) = arguments.split_first().ok_or_else(transport::invalid)?;
    let mut command = Command::new(program);
    command.args(arguments);
    Ok(command)
}
fn code_program() -> &'static str {
    if cfg!(windows) {
        "code.cmd"
    } else {
        "code"
    }
}
fn require_remote_ssh() -> Result<(), NativeError> {
    let mut command = Command::new(code_program());
    command.arg("--list-extensions");
    let extensions = ssh::bounded_output(&mut command, None).map_err(|_| {
        NativeError::new(
            "editor_unavailable",
            "Veranda could not check VS Code. Install its code command and try again.",
        )
    })?;
    let installed = std::str::from_utf8(&extensions).is_ok_and(|output| {
        output.lines().any(|line| {
            line.trim()
                .eq_ignore_ascii_case("ms-vscode-remote.remote-ssh")
        })
    });
    if installed {
        Ok(())
    } else {
        Err(NativeError::new(
            "remote_ssh_missing",
            "Install the Remote - SSH extension in VS Code, then open the project again.",
        ))
    }
}
fn terminal(command: Command) -> Result<Command, NativeError> {
    #[cfg(target_os = "linux")]
    {
        for (program, prefix) in [
            ("xterm", &["-e"][..]),
            ("gnome-terminal", &["--wait", "--"][..]),
            ("konsole", &["--nofork", "-e"][..]),
            ("xfce4-terminal", &["--disable-server", "-x"][..]),
        ] {
            if available(program) {
                let mut terminal = Command::new(program);
                terminal
                    .args(prefix)
                    .arg(command.get_program())
                    .args(command.get_args());
                return Ok(terminal);
            }
        }
        Err(NativeError::new("terminal_missing", "Install xterm, GNOME Terminal, Konsole or XFCE Terminal on this client, then open the session again."))
    }
    #[cfg(target_os = "macos")]
    {
        let script = format!("tell application \"Terminal\"\nactivate\nset sessionTab to do script {}\ndelay 1\nrepeat while busy of sessionTab\ndelay 1\nend repeat\nend tell", serde_json::to_string(&display_command(&command)?).map_err(|_| transport::invalid())?);
        let mut terminal = Command::new("osascript");
        terminal.args(["-e", &script]);
        Ok(terminal)
    }
    #[cfg(target_os = "windows")]
    {
        use std::os::windows::process::CommandExt;
        let mut command = command;
        command.creation_flags(0x00000010);
        Ok(command)
    }
    #[cfg(not(any(target_os = "linux", target_os = "macos", target_os = "windows")))]
    {
        let _ = command;
        Err(transport::invalid())
    }
}
#[cfg(target_os = "linux")]
fn available(program: &str) -> bool {
    std::env::var_os("PATH").is_some_and(|path| {
        std::env::split_paths(&path).any(|directory| directory.join(program).is_file())
    })
}
pub fn display_command(command: &Command) -> Result<String, NativeError> {
    std::iter::once(command.get_program())
        .chain(command.get_args())
        .map(|value| {
            let value = value
                .to_str()
                .filter(|value| {
                    value.len() < 16384 && !value.chars().any(|character| character.is_control())
                })
                .ok_or_else(transport::invalid)?;
            #[cfg(windows)]
            {
                Ok(format!("\"{}\"", value.replace('"', "\"\"")))
            }
            #[cfg(not(windows))]
            {
                Ok(format!("'{}'", value.replace('\'', "'\\''")))
            }
        })
        .collect::<Result<Vec<_>, NativeError>>()
        .map(|arguments| arguments.join(" "))
}
fn ssh_config_path(path: &Path) -> Result<String, NativeError> {
    let value = path
        .to_str()
        .filter(|value| !value.chars().any(|character| character.is_control()))
        .ok_or_else(transport::invalid)?;
    Ok(format!(
        "\"{}\"",
        value
            .replace('\\', "/")
            .replace('"', "\\\"")
            .replace('%', "%%")
    ))
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn shared_launch_descriptors_bind_exact_context() {
        for (fixture, kind) in [
            (
                include_str!("../../../api/yard-rpc/v1/fixtures/session-shell.json"),
                "shell",
            ),
            (
                include_str!("../../../api/yard-rpc/v1/fixtures/session-vscode.json"),
                "vscode",
            ),
        ] {
            let value: serde_json::Value = serde_json::from_str(fixture).unwrap();
            let descriptor: Descriptor = serde_json::from_value(value["result"].clone()).unwrap();
            descriptor
                .validate(kind, "default", false, Some("Demo"))
                .unwrap();
            assert!(descriptor
                .validate(kind, "another", false, Some("Demo"))
                .is_err());
        }
    }
    #[test]
    fn owner_arguments_cannot_request_arbitrary_execution() {
        let descriptor: Descriptor = serde_json::from_value(serde_json::json!({"schemaVersion":1,"kind":"shell","scope":"host","yardName":"default","ownerArguments":["sh","-c","anything"]})).unwrap();
        assert!(descriptor.validate("shell", "default", true, None).is_err());
    }
    #[test]
    fn copy_shell_command_prepares_the_owner_invocation_without_a_terminal() {
        let value: serde_json::Value = serde_json::from_str(include_str!(
            "../../../api/yard-rpc/v1/fixtures/session-shell.json"
        ))
        .unwrap();
        let descriptor: Descriptor = serde_json::from_value(value["result"].clone()).unwrap();
        descriptor
            .validate("shell", "default", false, Some("Demo"))
            .unwrap();
        let command = shell_command(descriptor, None).unwrap().command;
        assert!(
            command.contains("yard") && command.contains("default") && command.contains("Demo")
        );
        assert!(!command.contains("terminal") && !command.contains("xterm"));
    }
}
