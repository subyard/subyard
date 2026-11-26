use super::*;

#[test]
fn scrub_keeps_words_that_cannot_carry_values() {
    let cases = [
        (
            "yard: config validate failed",
            "yard: config validate failed",
        ),
        (
            "set HOST_LINKS=/srv/data now",
            "set HOST_LINKS=<redacted> now",
        ),
        ("flag --token=abc", "flag token=<redacted>"),
        (
            "profile \"secret value\" is 'x y' and `z`",
            "profile <redacted> is <redacted> and <redacted>",
        ),
        ("open /home/operator/.ssh/id failed", "open <path> failed"),
        (
            "ssh: dev@example.test: Permission denied",
            "ssh: <path> Permission denied",
        ),
        (
            "token ghp_0123456789abcdefABCD, done",
            "token <redacted> done",
        ),
        ("unterminated \"quote with value", "unterminated <redacted>"),
        ("bell\u{7}and\ttab", "bell and tab"),
        ("   ", ""),
    ];
    for (input, want) in cases {
        assert_eq!(scrub(input).unwrap_or_default(), want, "{input}");
    }
    let long = scrub(&"word ".repeat(200)).unwrap();
    assert!(long.len() <= MAX_LINE && long.ends_with('…'));
}

fn root() -> (PathBuf, impl Drop) {
    struct Remove(PathBuf);
    impl Drop for Remove {
        fn drop(&mut self) {
            let _ = std::fs::remove_dir_all(&self.0);
        }
    }
    let mut random = [0u8; 8];
    getrandom::fill(&mut random).unwrap();
    let id: String = random.iter().map(|byte| format!("{byte:02x}")).collect();
    let base = std::fs::canonicalize(std::env::temp_dir())
        .unwrap()
        .join(format!("veranda-diagnostics-test-{id}"));
    (base.join("store"), Remove(base))
}

#[test]
fn log_keeps_the_newest_bounded_records_privately() {
    let (root, _cleanup) = root();
    let log = DiagnosticLog::new(root);
    for index in 0..(MAX_RECORDS + 6) {
        log.record(
            "owner-failure",
            "system.ping",
            "disconnected",
            vec![format!("line {index} {}", "x".repeat(1000)); 40],
        );
    }
    let records = log.records();
    assert!(records.len() <= MAX_RECORDS && !records.is_empty());
    assert!(records
        .iter()
        .all(|record| record.detail.len() == TAIL_LINES
            && record.detail.iter().all(|line| line.len() <= MAX_LINE)));
    assert!(records.last().unwrap().detail[0].starts_with(&format!("line {}", MAX_RECORDS + 5)));
    assert!(std::fs::metadata(log.path()).unwrap().len() as usize <= MAX_FILE);
    log.record("bad kind!", "", "x/y", vec![]);
    let last = log.records().pop().unwrap();
    assert_eq!(
        (last.kind.as_str(), last.method.as_str(), last.code.as_str()),
        ("unknown", "unknown", "unknown")
    );
}

#[cfg(unix)]
#[test]
fn an_unsafe_or_oversized_log_is_replaced_not_trusted() {
    use std::os::unix::fs::PermissionsExt;
    let (root, _cleanup) = root();
    let log = DiagnosticLog::new(root.clone());
    log.record("owner-failure", "system.ping", "disconnected", vec![]);
    std::fs::set_permissions(log.path(), std::fs::Permissions::from_mode(0o644)).unwrap();
    assert!(log.records().is_empty());
    std::fs::remove_file(log.path()).unwrap();
    std::fs::write(log.path(), vec![b'x'; MAX_FILE + 1]).unwrap();
    std::fs::set_permissions(log.path(), std::fs::Permissions::from_mode(0o600)).unwrap();
    assert!(log.records().is_empty());
    log.record("owner-failure", "system.ping", "disconnected", vec![]);
    assert_eq!(log.records().len(), 1);
}
