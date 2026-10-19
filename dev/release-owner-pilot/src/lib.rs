#![forbid(unsafe_code)]
//! Consent handles are local phase values, not deserializable authority.
//! ```
//! use release_owner_pilot::domain::Prepared;
//! let p=Prepared::new("a".repeat(64),"b".repeat(64),"named".into(),serde_json::json!({}),100).unwrap();
//! let token=p.plan().to_owned();let _record=p.consent(&token,101).unwrap().consume();
//! ```
//! ```compile_fail,E0382
//! # use release_owner_pilot::domain::Consented;
//! fn replay(c:Consented){let _=c.consume();let _=c.consume();}
//! ```
//! ```compile_fail,E0277
//! # use release_owner_pilot::domain::Consented;
//! let _:Consented=serde_json::from_str("{}").unwrap();
//! ```
//! ```compile_fail,E0451
//! # use release_owner_pilot::domain::{Prepared,Record};
//! fn forge(record:Record){let _=Prepared{record};}
//! ```
pub mod domain;
pub mod protocol;
use sha2::{Digest, Sha256};
#[derive(Debug)]
pub struct Failure(pub &'static str);
pub type Result<T> = std::result::Result<T, Failure>;
pub fn digest(bytes: &[u8]) -> String {
    Sha256::digest(bytes)
        .iter()
        .map(|b| format!("{b:02x}"))
        .collect()
}
pub fn canonical(value: &impl serde::Serialize) -> Result<Vec<u8>> {
    let v = serde_json::to_value(value).map_err(|_| Failure("codec"))?;
    serde_json::to_vec(&v).map_err(|_| Failure("codec"))
}
