fn main() {
    println!("cargo:rerun-if-env-changed=VERANDA_PRODUCT_VERSION");
    if let Ok(version) = std::env::var("VERANDA_PRODUCT_VERSION") {
        let parts: Vec<&str> = version.split('.').collect();
        assert!(
            parts.len() == 3
                && parts.iter().all(|part| {
                    !part.is_empty()
                        && part.bytes().all(|byte| byte.is_ascii_digit())
                        && (part.len() == 1 || !part.starts_with('0'))
                }),
            "VERANDA_PRODUCT_VERSION must be stable MAJOR.MINOR.PATCH"
        );
        println!("cargo:rustc-env=VERANDA_PRODUCT_VERSION={version}");
    }
}
