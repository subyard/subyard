//! Canonical Go JSON encoding, separate from raw frozen decoder admission.
use crate::{Error, Result};
use serde::{de::DeserializeOwned, Serialize};
use serde_json::ser::{CompactFormatter, Formatter};
use std::io::{self, Write};
pub const MAX_BYTES: usize = 1 << 20;
struct GoFormatter;
impl Formatter for GoFormatter {
    fn write_string_fragment<W: ?Sized + Write>(
        &mut self,
        out: &mut W,
        value: &str,
    ) -> io::Result<()> {
        for c in value.chars() {
            match c {
                '<' => out.write_all(b"\\u003c")?,
                '>' => out.write_all(b"\\u003e")?,
                '&' => out.write_all(b"\\u0026")?,
                '\u{2028}' => out.write_all(b"\\u2028")?,
                '\u{2029}' => out.write_all(b"\\u2029")?,
                _ => CompactFormatter.write_string_fragment(out, c.encode_utf8(&mut [0; 4]))?,
            }
        }
        Ok(())
    }
}
pub fn encode<T: Serialize>(value: &T, newline: bool) -> Result<Vec<u8>> {
    encode_limit(value, newline, MAX_BYTES)
}
// Inventory has its own fixed entry/physical-byte bounds; it is not a record.
pub(crate) fn encode_inventory<T: Serialize>(value: &T) -> Result<Vec<u8>> {
    encode_limit(value, false, 8 << 20)
}
pub(crate) fn encode_bundle<T: Serialize>(value: &T) -> Result<Vec<u8>> {
    encode_limit(value, false, 64 << 20)
}
pub(crate) fn encode_native_recipe<T: Serialize>(value: &T) -> Result<Vec<u8>> {
    encode_limit(value, false, 96 << 20)
}
fn encode_limit<T: Serialize>(value: &T, newline: bool, maximum: usize) -> Result<Vec<u8>> {
    let mut bytes = Vec::new();
    value
        .serialize(&mut serde_json::Serializer::with_formatter(
            &mut bytes,
            GoFormatter,
        ))
        .map_err(|_| Error::Codec)?;
    if newline {
        bytes.push(b'\n');
    }
    if bytes.is_empty() || bytes.len() > maximum {
        return Err(Error::Bound);
    }
    Ok(bytes)
}
/// Canonical-only admission. This is deliberately not the permissive frozen Go decoder.
/// Production routing must not use it as a replacement for that decoder.
pub fn decode_canonical<T: DeserializeOwned + Serialize>(bytes: &[u8], newline: bool) -> Result<T> {
    if bytes.is_empty() || bytes.len() > MAX_BYTES {
        return Err(Error::Bound);
    }
    let value = serde_json::from_slice(bytes).map_err(|_| Error::Codec)?;
    if encode(&value, newline)? != bytes {
        return Err(Error::Noncanonical);
    }
    Ok(value)
}
