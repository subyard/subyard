//! Which Subyard engines this Veranda admits. `veranda/compatibility.json`
//! records the same window; capabilities alone gate features inside it.
use crate::local_fleet::NativeError;
use crate::vocabulary::Capability;

/// The engine release line this Veranda is released with.
pub const ENGINE_CURRENT_LINE: &str = "0.33";
/// The previous published engine release line, the oldest one admitted.
pub const ENGINE_PREVIOUS_LINE: &str = "0.32";
const REQUIRED: &str = Capability::OwnerInventoryV1.as_str();

/// Admits an engine whose version equals this Veranda's own version (same-build
/// development and test pairs) or lies on the current or previous release line,
/// and which advertises the capability every session needs. Every refusal it
/// determines is reported in one error.
pub fn admit(own: &str, engine: &str, capabilities: &[String]) -> Result<(), NativeError> {
    let release = stable(engine);
    let admitted = engine == own
        || release.is_some_and(|(major, minor)| {
            let line = format!("{major}.{minor}");
            line == ENGINE_CURRENT_LINE || line == ENGINE_PREVIOUS_LINE
        });
    let capable = capabilities.iter().any(|capability| capability == REQUIRED);
    if admitted && capable {
        return Ok(());
    }
    let mut text = Vec::new();
    if !admitted {
        text.push(format!(
            "Veranda {own} supports Subyard {ENGINE_CURRENT_LINE} and {ENGINE_PREVIOUS_LINE}; this owner runs Subyard {engine}."
        ));
    }
    if !capable {
        text.push(format!("Subyard {engine} does not support {REQUIRED}."));
    }
    let newer = release.is_some_and(|line| line_newer(line, ENGINE_CURRENT_LINE));
    text.push(
        if !admitted && newer {
            "Update Veranda."
        } else {
            "Install a supported Subyard release on the owner."
        }
        .into(),
    );
    let code = if admitted {
        "capability_missing"
    } else {
        "incompatible_engine"
    };
    Err(NativeError::new(code, text.join(" ")))
}

/// The release line of a stable `MAJOR.MINOR.PATCH` version: numeric parts
/// without leading zeros, as `build.rs` requires of Veranda's own version.
/// Anything else, including a part that overflows, is not a release.
fn stable(version: &str) -> Option<(u64, u64)> {
    let parts: Vec<&str> = version.split('.').collect();
    if parts.len() != 3 {
        return None;
    }
    let mut numbers = [0u64; 3];
    for (number, part) in numbers.iter_mut().zip(&parts) {
        if part.is_empty()
            || !part.bytes().all(|byte| byte.is_ascii_digit())
            || (part.len() > 1 && part.starts_with('0'))
        {
            return None;
        }
        *number = part.parse().ok()?;
    }
    Some((numbers[0], numbers[1]))
}

fn line_newer((major, minor): (u64, u64), line: &str) -> bool {
    let (current_major, current_minor) = line
        .split_once('.')
        .and_then(|(major, minor)| Some((major.parse::<u64>().ok()?, minor.parse::<u64>().ok()?)))
        .expect("the current engine line is MAJOR.MINOR");
    (major, minor) > (current_major, current_minor)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn caps(required: bool) -> Vec<String> {
        let mut caps = vec!["future-capability".to_string()];
        if required {
            caps.push(REQUIRED.into());
        }
        caps
    }

    #[test]
    fn admission_window_and_texts() {
        let older = |own: &str, engine: &str| {
            format!("Veranda {own} supports Subyard 0.33 and 0.32; this owner runs Subyard {engine}. Install a supported Subyard release on the owner.")
        };
        let newer = |own: &str, engine: &str| {
            format!("Veranda {own} supports Subyard 0.33 and 0.32; this owner runs Subyard {engine}. Update Veranda.")
        };
        for engine in ["0.33.0", "0.33.7", "0.32.0", "0.32.12"] {
            assert!(admit("0.33.0", engine, &caps(true)).is_ok(), "{engine}");
            // A development Veranda admits the release lines too.
            assert!(admit("0.1.0-dev", engine, &caps(true)).is_ok(), "{engine}");
        }
        for (own, engine) in [("0.1.0-dev", "0.1.0-dev"), ("0.99.1", "0.99.1")] {
            assert!(admit(own, engine, &caps(true)).is_ok(), "{engine}");
        }
        for (own, engine, text) in [
            ("0.33.0", "0.31.9", older("0.33.0", "0.31.9")),
            ("0.33.0", "0.9.0", older("0.33.0", "0.9.0")),
            ("0.99.1", "0.99.2", newer("0.99.1", "0.99.2")),
            ("0.33.0", "0.34.0", newer("0.33.0", "0.34.0")),
            ("0.33.0", "1.0.0", newer("0.33.0", "1.0.0")),
            ("0.33.0", "0.1.0-dev", older("0.33.0", "0.1.0-dev")),
            ("0.33.0", "0.34.0-rc.1", older("0.33.0", "0.34.0-rc.1")),
            ("0.33.0", "0.033.0", older("0.33.0", "0.033.0")),
            ("0.33.0", "0.33.0.1", older("0.33.0", "0.33.0.1")),
            ("0.33.0", "0.33.0+x", older("0.33.0", "0.33.0+x")),
            ("0.33.0", "v0.33.0", older("0.33.0", "v0.33.0")),
            ("0.33.0", "0.33.", older("0.33.0", "0.33.")),
        ] {
            let error = admit(own, engine, &caps(true)).unwrap_err();
            assert_eq!(
                (error.code.as_str(), error.message),
                ("incompatible_engine", text),
                "{engine}"
            );
        }
        let huge = format!("0.{}.0", "9".repeat(30));
        let error = admit("0.33.0", &huge, &caps(true)).unwrap_err();
        assert_eq!(error.message, older("0.33.0", &huge));
    }

    #[test]
    fn independent_refusals_are_reported_together() {
        let error = admit("0.33.0", "0.32.4", &caps(false)).unwrap_err();
        assert_eq!(
            (error.code.as_str(), error.message.as_str()),
            ("capability_missing", "Subyard 0.32.4 does not support owner-inventory-v1. Install a supported Subyard release on the owner.")
        );
        let error = admit("0.33.0", "0.31.0", &caps(false)).unwrap_err();
        assert_eq!(
            (error.code.as_str(), error.message.as_str()),
            ("incompatible_engine", "Veranda 0.33.0 supports Subyard 0.33 and 0.32; this owner runs Subyard 0.31.0. Subyard 0.31.0 does not support owner-inventory-v1. Install a supported Subyard release on the owner.")
        );
        let error = admit("0.33.0", "0.34.0", &caps(false)).unwrap_err();
        assert!(error
            .message
            .ends_with("does not support owner-inventory-v1. Update Veranda."));
    }
}
