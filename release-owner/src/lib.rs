#![forbid(unsafe_code)]
//! Release owner with an inert private operation entrypoint. No product writer
//! capability or default route is enabled.
mod activation;
mod assets;
mod bundle;
pub mod codec;
mod custody;
mod execution;
#[doc(hidden)]
pub mod fixture;
pub mod graph;
pub mod history;
mod installed_recipe;
pub mod inventory;
pub mod lease;
mod ledger_execution;
pub mod legacy;
pub mod links;
pub mod migration_owner;
mod native;
mod native_recovery;
mod native_retention;
mod native_transaction_retention;
mod ordinary;
mod outer_custody;
pub mod owner;
mod owner_cleanup;
mod owner_publication;
pub mod plan;
mod prepared_payload;
pub mod record;
pub mod recovery;
mod reducer;
mod rollback;
mod settings;
pub mod source;
pub mod source_admission;
pub mod store;
pub mod wire;
mod work;
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Error {
    Invalid,
    Bound,
    Codec,
    Noncanonical,
    Io,
    Protected,
    Stale,
    Exists,
    Unknown,
}
pub type Result<T> = std::result::Result<T, Error>;
pub fn digest(bytes: &[u8]) -> String {
    use sha2::{Digest, Sha256};
    Sha256::digest(bytes)
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect()
}

#[cfg(test)]
mod recovery_tests;

pub mod recovery_operation;

pub mod migration_operation;

#[cfg(test)]
mod operation_oracle;
