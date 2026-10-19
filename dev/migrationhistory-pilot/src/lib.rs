#![forbid(unsafe_code)]
//! Dev-only history experiment. No authorization, persistence or runtime readiness.
//!
//! Positive control: borrowing a snapshot and consuming a semantic edge.
//! ```
//! use migrationhistory_pilot::{Registry, RegistryInput};
//! let input: RegistryInput = serde_json::from_str(r#"{"minimum":{"a":1},"current":{"a":2},"migrations":[{"id":"a2","domain":"a","fromEpoch":1,"toEpoch":2,"kind":"kind"}]}"#).unwrap();
//! let registry = Registry::new(input).unwrap();
//! let base = registry.baseline();
//! assert_eq!(base.domains()["a"].epoch(), 1);
//! let edge = registry.next(&base).unwrap();
//! assert_eq!(edge.migration().id, "a2");
//! let after = edge.into_after();
//! assert!(registry.complete(&after).is_ok());
//! ```
//! A borrowed view cannot mutate the checked frontier (E0596).
//! ```compile_fail,E0596
//! # use migrationhistory_pilot::Authority;
//! fn corrupt(a: &Authority) { a.domains().clear(); }
//! ```
//! A consumed semantic step cannot be reused (E0382).
//! ```compile_fail,E0382
//! # use migrationhistory_pilot::Edge;
//! fn twice(edge: Edge) { let _ = edge.into_after(); let _ = edge.into_after(); }
//! ```
//! Projection cannot serve as planning authority (E0308).
//! ```compile_fail,E0308
//! # use migrationhistory_pilot::{Registry, CompatibilityProjection};
//! fn plan(r: &Registry, p: &CompatibilityProjection) { let _ = r.next(p); }
//! ```
//! Checked values cannot be forged by their fields (E0451).
//! ```compile_fail,E0451
//! # use migrationhistory_pilot::CompatibilityProjection;
//! let forged = CompatibilityProjection { epochs: Default::default(), pin: String::new() };
//! ```
//! DTO deserialization does not create a checked authority (E0277).
//! ```compile_fail,E0277
//! # use migrationhistory_pilot::Authority;
//! let _: Authority = serde_json::from_str("{}").unwrap();
//! ```

pub use subyard_release_owner::history::*;
