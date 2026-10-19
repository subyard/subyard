pub mod checkpoint_v1;
pub mod journal_v2;
pub mod ledger_v2;
pub mod process_v1;
pub mod registry_v2;
pub fn empty_vec<T>(v: &Option<Vec<T>>) -> bool {
    v.as_ref().is_none_or(Vec::is_empty)
}
pub fn empty_map<K, V>(v: &Option<std::collections::BTreeMap<K, V>>) -> bool {
    v.as_ref().is_none_or(std::collections::BTreeMap::is_empty)
}

pub mod lifecycle_v1;
pub mod migration_owner_v1;
pub mod plan_v1;
pub mod process_v2;
pub mod recovery_v1;
pub fn zero<T: Default + PartialEq>(value: &T) -> bool {
    value == &T::default()
}
pub mod go_bytes {
    use base64::{engine::general_purpose::STANDARD, Engine};
    use serde::{Deserialize, Deserializer, Serializer};
    pub fn serialize<S: Serializer>(
        value: &Option<Vec<u8>>,
        serializer: S,
    ) -> Result<S::Ok, S::Error> {
        match value {
            None => serializer.serialize_none(),
            Some(bytes) => serializer.serialize_str(&STANDARD.encode(bytes)),
        }
    }
    pub fn deserialize<'de, D: Deserializer<'de>>(
        deserializer: D,
    ) -> Result<Option<Vec<u8>>, D::Error> {
        let value = Option::<String>::deserialize(deserializer)?;
        value
            .map(|s| STANDARD.decode(s).map_err(serde::de::Error::custom))
            .transpose()
    }
}
