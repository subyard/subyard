//! Canonical Go JSON encoding, separate from raw frozen decoder admission.
use crate::{Error, Result};
use serde::{de::DeserializeOwned, Serialize};
#[cfg(test)]
use serde_json::ser::CompactFormatter;
use serde_json::ser::Formatter;
use std::io::{self, Write};
pub const MAX_BYTES: usize = 1 << 20;
struct GoFormatter;
// Source inventories can contain every manifest-listed path. They are not
// protected records or protocol frames; stream their finite canonical encoding.
pub(crate) fn source_fingerprint<T: Serialize>(value: &T) -> Result<String> {
    source_hash_with(|out| write_source_json(out, value))
}
pub(crate) fn source_hash_with(write: impl FnOnce(&mut dyn Write) -> Result<()>) -> Result<String> {
    use sha2::{Digest, Sha256};
    struct Hash {
        sha: Sha256,
        bytes: usize,
    }
    impl Write for Hash {
        fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
            self.bytes = self
                .bytes
                .checked_add(bytes.len())
                .ok_or_else(|| io::Error::other("source inventory bound"))?;
            self.sha.update(bytes);
            Ok(bytes.len())
        }
        fn flush(&mut self) -> io::Result<()> {
            Ok(())
        }
    }
    let mut hash = Hash {
        sha: Sha256::new(),
        bytes: 0,
    };
    write(&mut hash)?;
    Ok(hash
        .sha
        .finalize()
        .iter()
        .map(|b| format!("{b:02x}"))
        .collect())
}
pub(crate) fn write_source_json<T: Serialize>(out: &mut dyn Write, value: &T) -> Result<()> {
    value
        .serialize(&mut serde_json::Serializer::with_formatter(
            out,
            GoFormatter,
        ))
        .map_err(|_| Error::Codec)
}
// Only the fixed physical source inventories have original OS-byte map keys.
// Preserve byte sorting and Go's escaped RuneError for EACH invalid byte.
pub(crate) fn write_source_map<T: Serialize>(
    out: &mut dyn Write,
    values: &std::collections::BTreeMap<Vec<u8>, T>,
) -> Result<()> {
    fn fragment(out: &mut dyn Write, valid: &str) -> Result<()> {
        let quoted = encode(&valid, false)?;
        out.write_all(&quoted[1..quoted.len() - 1])
            .map_err(|_| Error::Io)
    }
    out.write_all(b"{").map_err(|_| Error::Io)?;
    for (index, (key, value)) in values.iter().enumerate() {
        if index != 0 {
            out.write_all(b",").map_err(|_| Error::Io)?;
        }
        out.write_all(b"\"").map_err(|_| Error::Io)?;
        let mut raw = key.as_slice();
        while !raw.is_empty() {
            match std::str::from_utf8(raw) {
                Ok(valid) => {
                    fragment(out, valid)?;
                    break;
                }
                Err(error) => {
                    let valid = error.valid_up_to();
                    fragment(
                        out,
                        std::str::from_utf8(&raw[..valid]).map_err(|_| Error::Invalid)?,
                    )?;
                    out.write_all(b"\\ufffd").map_err(|_| Error::Io)?;
                    raw = &raw[valid + 1..];
                }
            }
        }
        out.write_all(b"\":").map_err(|_| Error::Io)?;
        write_source_json(out, value)?;
    }
    out.write_all(b"}").map_err(|_| Error::Io)
}
impl Formatter for GoFormatter {
    fn write_string_fragment<W: ?Sized + Write>(
        &mut self,
        out: &mut W,
        value: &str,
    ) -> io::Result<()> {
        let mut start = 0;
        for (index, c) in value.char_indices() {
            let escaped: &[u8] = match c {
                '<' => b"\\u003c",
                '>' => b"\\u003e",
                '&' => b"\\u0026",
                '\u{2028}' => b"\\u2028",
                '\u{2029}' => b"\\u2029",
                _ => continue,
            };
            out.write_all(value[start..index].as_bytes())?;
            out.write_all(escaped)?;
            start = index + c.len_utf8();
        }
        out.write_all(value[start..].as_bytes())
    }
}
pub fn encode<T: Serialize>(value: &T, newline: bool) -> Result<Vec<u8>> {
    encode_limit(value, newline, MAX_BYTES)
}
pub(crate) fn encode_source_proof<T: Serialize>(value: &T) -> Result<Vec<u8>> {
    // One original1MiB journal carried as Go []byte needs4/3MiB base64.
    encode_limit(value, true, 2 << 20)
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
// Hash the same canonical recipe bytes without retaining a second full copy.
pub(crate) fn hash_native_recipe<T: Serialize>(value: &T) -> Result<String> {
    struct Bounded<'a> {
        out: &'a mut dyn Write,
        written: usize,
        exceeded: &'a mut bool,
    }
    impl Write for Bounded<'_> {
        fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
            let Some(total) = self.written.checked_add(bytes.len()) else {
                *self.exceeded = true;
                return Err(io::Error::other("recipe bound"));
            };
            if total > 96 << 20 {
                *self.exceeded = true;
                return Err(io::Error::other("recipe bound"));
            }
            self.out.write_all(bytes)?;
            self.written = total;
            Ok(bytes.len())
        }
        fn flush(&mut self) -> io::Result<()> {
            self.out.flush()
        }
    }
    let mut exceeded = false;
    let mut empty = false;
    let result = source_hash_with(|out| {
        let mut bounded = Bounded {
            out,
            written: 0,
            exceeded: &mut exceeded,
        };
        let result = write_source_json(&mut bounded, value);
        empty = bounded.written == 0;
        result
    });
    if exceeded || empty && result.is_ok() {
        return Err(Error::Bound);
    }
    result
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

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn streamed_recipe_hash_matches_exact_bytes_and_bound() {
        struct Empty;
        impl Serialize for Empty {
            fn serialize<S: serde::Serializer>(
                &self,
                _serializer: S,
            ) -> std::result::Result<S::Ok, S::Error> {
                Err(serde::ser::Error::custom("fixture serialization failure"))
            }
        }
        assert!(matches!(hash_native_recipe(&Empty), Err(Error::Codec)));
        let long = "A/+=".repeat(262144);
        for value in ["", "<>&\u{2028}\u{2029}\"\\\n😀", long.as_str()] {
            let raw = encode_native_recipe(&value).unwrap();
            assert_eq!(hash_native_recipe(&value).unwrap(), crate::digest(&raw));
        }
        let at_limit = "a".repeat((96 << 20) - 2);
        let raw = encode_native_recipe(&at_limit).unwrap();
        assert_eq!(raw.len(), 96 << 20);
        assert_eq!(hash_native_recipe(&at_limit).unwrap(), crate::digest(&raw));
        drop(raw);
        let over_limit = at_limit + "a";
        assert!(matches!(hash_native_recipe(&over_limit), Err(Error::Bound)));
        assert!(matches!(
            encode_native_recipe(&over_limit),
            Err(Error::Bound)
        ));
    }
    #[test]
    fn bulk_fragments_match_original_formatter_for_large_and_mixed_strings() {
        struct Original;
        impl Formatter for Original {
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
                        _ => CompactFormatter
                            .write_string_fragment(out, c.encode_utf8(&mut [0; 4]))?,
                    }
                }
                Ok(())
            }
        }
        for value in [
            "A/+=".repeat(262144),
            "é<&>\u{2028}\u{2029}\"\\\n😀".repeat(1000),
            String::new(),
        ] {
            let mut previous = Vec::new();
            value
                .serialize(&mut serde_json::Serializer::with_formatter(
                    &mut previous,
                    Original,
                ))
                .unwrap();
            assert_eq!(encode_native_recipe(&value).unwrap(), previous);
        }
    }
    #[test]
    fn source_proof_carries_an_original_maximum_record() {
        let proof = crate::source_query::Proof {
            journal_raw: Some(vec![b'a'; MAX_BYTES]),
            metadata_binding: "a".repeat(64),
            binding: "b".repeat(64),
            retained_releases: Vec::new(),
        };
        let raw = encode_source_proof(&proof).unwrap();
        assert!(raw.len() > MAX_BYTES && raw.len() < 2 << 20);
        let decoded: crate::source_query::Proof = serde_json::from_slice(&raw).unwrap();
        assert_eq!(decoded.journal_raw, proof.journal_raw);
    }
    #[test]
    fn actual_go_source_path_maps() {
        use base64::{engine::general_purpose::STANDARD, Engine};
        let Ok(path) = std::env::var("SUBYARD_RUST_SOURCE_BINDING_CORPUS") else {
            return;
        };
        #[derive(serde::Deserialize)]
        struct Row {
            name: String,
            keys: Vec<String>,
            values: Vec<u64>,
            sha256: String,
        }
        let rows: Vec<Row> = serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
        assert_eq!(rows.len(), 2);
        for row in rows {
            assert_eq!(row.keys.len(), row.values.len());
            let values = row
                .keys
                .into_iter()
                .zip(row.values)
                .map(|(key, value)| (STANDARD.decode(key).unwrap(), value))
                .collect();
            assert_eq!(
                source_hash_with(|out| write_source_map(out, &values)).unwrap(),
                row.sha256,
                "{}",
                row.name
            );
        }
    }
}
