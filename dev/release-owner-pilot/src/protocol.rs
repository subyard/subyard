use serde::{Deserialize, Serialize};
use serde_json::Value;
#[derive(Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Operation {
    Prepare,
    Observe,
    Apply,
}
#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Request {
    pub version: u32,
    pub op: Operation,
    pub yard: String,
    pub recipe: Option<Value>,
    pub pause: String,
}
#[derive(Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Status {
    Prepared,
    Expected,
    Desired,
    Conflict,
}
#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Response {
    pub version: u32,
    pub status: Status,
    pub recipe: Option<Value>,
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn golden() {
        let bytes = include_bytes!("../protocol-v1.golden.json");
        let r: Request = serde_json::from_slice(bytes).unwrap();
        assert_eq!(
            crate::canonical(&r).unwrap(),
            bytes.strip_suffix(b"\n").unwrap()
        );
        assert!(serde_json::from_str::<Request>(
            r#"{"op":"execute","pause":"","recipe":null,"version":1,"yard":"named"}"#
        )
        .is_err());
        assert!(serde_json::from_str::<Response>(
            r#"{"version":1,"status":"desired","recipe":null,"extra":true}"#
        )
        .is_err());
    }
}
