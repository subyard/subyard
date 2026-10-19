use crate::{canonical, digest, Failure, Result};
use serde::{Deserialize, Serialize};
use serde_json::Value;
#[derive(Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum Phase {
    Intent,
    Verified,
}
#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Record {
    pub version: u32,
    pub owner: String,
    pub helper: String,
    pub transaction: String,
    pub plan: String,
    pub created: u64,
    pub expires: u64,
    pub yard: String,
    pub recipe: Value,
    pub phase: Phase,
}
pub struct Prepared {
    record: Record,
}
pub struct Consented {
    record: Record,
}
impl Prepared {
    pub fn new(
        owner: String,
        helper: String,
        yard: String,
        recipe: Value,
        now: u64,
    ) -> Result<Self> {
        let recipe_digest = digest(&canonical(&recipe)?);
        let transaction = digest(
            format!(
                "{owner}:{helper}:{recipe_digest}:{now}:{}",
                std::process::id()
            )
            .as_bytes(),
        );
        let expires = now.checked_add(300).ok_or(Failure("expiry"))?;
        let mut record = Record {
            version: 1,
            owner,
            helper,
            transaction,
            plan: String::new(),
            created: now,
            expires,
            yard,
            recipe,
            phase: Phase::Intent,
        };
        record.plan = record.plan_binding()?;
        Ok(Self { record })
    }

    pub fn plan(&self) -> &str {
        &self.record.plan
    }
    pub fn consent(self, token: &str, now: u64) -> Result<Consented> {
        if token != self.record.plan || now > self.record.expires || now < self.record.created {
            return Err(Failure("consent"));
        };
        Ok(Consented {
            record: self.record,
        })
    }
}
impl Consented {
    pub fn consume(self) -> Record {
        self.record
    }
}
impl Record {
    pub fn check_fresh(&self, now: u64) -> Result<()> {
        if now < self.created || now > self.expires {
            return Err(Failure("expiry"));
        };
        Ok(())
    }

    fn plan_binding(&self) -> Result<String> {
        Ok(digest(&canonical(
            &serde_json::json!({"version":self.version,"owner":self.owner,"helper":self.helper,"transaction":self.transaction,"created":self.created,"expires":self.expires,"yard":self.yard,"recipe":self.recipe}),
        )?))
    }

    pub fn check(&self, owner: &str, helper: &str, yard: &str) -> Result<()> {
        if self.version != 1
            || self.owner != owner
            || self.helper != helper
            || self.yard != yard
            || self.plan != self.plan_binding()?
            || self.transaction.len() != 64
        {
            return Err(Failure("record-binding"));
        };
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn prepared() -> Prepared {
        Prepared::new(
            "a".repeat(64),
            "b".repeat(64),
            "named".into(),
            serde_json::json!({"version":1}),
            100,
        )
        .unwrap()
    }
    #[test]
    fn consent_contract() {
        let p = prepared();
        let token = p.plan().to_owned();
        let r = p.consent(&token, 101).unwrap().consume();
        assert!(r.check(&"a".repeat(64), &"b".repeat(64), "named").is_ok());
        assert!(r.check(&"a".repeat(64), &"c".repeat(64), "named").is_err());
        assert!(prepared().consent("foreign", 101).is_err());
        assert!(r.check_fresh(401).is_err());
        let p = prepared();
        let token = p.plan().to_owned();
        assert!(p.consent(&token, 401).is_err());
        assert!(Prepared::new(
            String::new(),
            String::new(),
            String::new(),
            Value::Null,
            u64::MAX
        )
        .is_err());
    }
}
