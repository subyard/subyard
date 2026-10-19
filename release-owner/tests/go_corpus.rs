use serde::Deserialize;
use subyard_release_owner::{codec, digest, record, wire, Result};
#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Corpus {
    catalog: Vec<wire::registry_v2::CapabilityDescriptor>,
    catalog_digest: String,
    registry: String,
    registry_digest: String,
    projection: String,
    vectors: Vec<Vector>,
    checkpoint_views: Vec<CheckpointView>,
}
#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct CheckpointView {
    name: String,
    registry: String,
    registry_digest: String,
    checkpoint: String,
    accepted: bool,
    ledger: String,
}
#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Vector {
    name: String,
    kind: String,
    input: String,
    accepted: bool,
    canonical: String,
    raw_digest: String,
    syntax_probe: bool,
}
fn process(
    kind: &str,
    bytes: &[u8],
    catalog: &record::Catalog,
    registry: &record::Registry,
    registry_digest: &str,
    projection: &record::Projection,
) -> Result<Vec<u8>> {
    macro_rules! checked {
        ($wire:ty,$constructor:path,$lf:expr) => {{
            let dto: $wire =
                serde_json::from_slice(bytes).map_err(|_| subyard_release_owner::Error::Codec)?;
            let value = $constructor(dto)?;
            codec::encode(value.wire(), $lf)
        }};
    }
    match kind {
        "catalog" => {
            let dto =
                serde_json::from_slice(bytes).map_err(|_| subyard_release_owner::Error::Codec)?;
            let value = record::Catalog::new(dto)?;
            codec::encode(value.descriptors(), false)
        }
        "registry" => {
            let dto =
                serde_json::from_slice(bytes).map_err(|_| subyard_release_owner::Error::Codec)?;
            let value = record::Registry::new(dto, catalog)?;
            codec::encode(value.wire(), false)
        }
        "ledger" => {
            let dto =
                serde_json::from_slice(bytes).map_err(|_| subyard_release_owner::Error::Codec)?;
            let value = record::Ledger::new(dto, registry)?;
            codec::encode(value.wire(), true)
        }
        "checkpoint" => {
            let dto =
                serde_json::from_slice(bytes).map_err(|_| subyard_release_owner::Error::Codec)?;
            let value = record::Checkpoint::new(dto, registry, registry_digest, projection)?;
            codec::encode(value.wire(), true)
        }
        "journal" => checked!(wire::journal_v2::Record, record::Journal::new, true),
        "evidence" => checked!(wire::journal_v2::Evidence, record::Evidence::new, true),
        "archive" => checked!(wire::journal_v2::Archive, record::Archive::new, true),
        "compatibility" => checked!(
            wire::journal_v2::CompatibilityEvidence,
            record::CompatibilityEvidence::new,
            true
        ),
        "request" => checked!(wire::process_v1::Request, record::Request::new, true),
        "response" => checked!(wire::process_v1::Response, record::Response::new, true),
        _ => panic!("unknown fixture kind"),
    }
}
#[test]
fn go_owned_frozen_formats() {
    let Ok(path) = std::env::var("SUBYARD_RUST_OWNER_CORPUS") else {
        eprintln!("Go corpus not supplied; run release-owner/check.sh");
        return;
    };
    let bytes = std::fs::read(path).unwrap();
    assert!(bytes.len() < 1 << 20);
    let corpus: Corpus = serde_json::from_slice(&bytes).unwrap();
    let catalog = record::Catalog::new(Some(corpus.catalog)).unwrap();
    assert_eq!(catalog.digest(), corpus.catalog_digest);
    let registry =
        record::Registry::new(serde_json::from_str(&corpus.registry).unwrap(), &catalog).unwrap();
    assert_eq!(digest(corpus.registry.as_bytes()), corpus.registry_digest);
    let projection = record::Projection::from_canonical(corpus.projection.into_bytes()).unwrap();
    let mut probes = 0;
    for vector in &corpus.vectors {
        assert_eq!(
            digest(vector.input.as_bytes()),
            vector.raw_digest,
            "{} raw pin",
            vector.name
        );
        let input = if vector.syntax_probe {
            probes += 1;
            if !vector.accepted {
                continue;
            }
            vector.canonical.as_bytes()
        } else {
            vector.input.as_bytes()
        };
        let actual = process(
            &vector.kind,
            input,
            &catalog,
            &registry,
            &corpus.registry_digest,
            &projection,
        );
        assert_eq!(
            actual.is_ok(),
            vector.accepted,
            "{}: {:?}",
            vector.name,
            actual
        );
        if vector.accepted {
            assert_eq!(
                actual.unwrap(),
                vector.canonical.as_bytes(),
                "{} canonical",
                vector.name
            );
        }
    }
    assert!(corpus.vectors.len() >= 40 && probes >= 12);
    assert_eq!(corpus.checkpoint_views.len(), 4);
    for case in &corpus.checkpoint_views {
        let registry =
            record::Registry::new(serde_json::from_str(&case.registry).unwrap(), &catalog).unwrap();
        let value = record::Checkpoint::new(
            serde_json::from_str(&case.checkpoint).unwrap(),
            &registry,
            &case.registry_digest,
            &projection,
        );
        assert_eq!(value.is_ok(), case.accepted, "{}", case.name);
        if let Ok(checkpoint) = value {
            let ledger = checkpoint.ledger_view();
            assert_eq!(
                codec::encode(ledger.wire(), true).unwrap(),
                case.ledger.as_bytes(),
                "{} actual history",
                case.name
            );
            assert!(
                registry.history().complete(ledger.authority()).is_ok(),
                "{} returned authority",
                case.name
            );
        }
    }
    println!(
        "{} Go format vectors; {probes} raw Go syntax probes (canonical Rust boundary only); 4 Go-owned checkpoint provenance/history cases",
        corpus.vectors.len()
    );
}
