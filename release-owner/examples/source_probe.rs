//! Opt-in owning test probe: read-only on the Go fixture's original inodes.
#![forbid(unsafe_code)]
use std::io::Read;
use subyard_release_owner::{codec, inventory::Namespace, store::Store, Error, Result};
fn run() -> Result<String> {
    let mut args = std::env::args().skip(1);
    let mode = args.next().ok_or(Error::Invalid)?;
    let path = std::path::PathBuf::from(args.next().ok_or(Error::Invalid)?);
    if args.next().is_some() || !path.starts_with("/tmp") {
        return Err(Error::Invalid);
    }
    let store = if let Ok(helper) = std::env::var("SUBYARD_RUST_CODEC_HELPER") {
        Store::open_with_reader(
            &path,
            subyard_release_owner::legacy::FrozenReader::capture_helper(
                std::fs::File::open(helper).map_err(|_| Error::Io)?,
            )?,
        )?
    } else {
        Store::open_canonical_fixture(&path)?
    };
    match mode.as_str() {
        "metadata" => Ok(store.source_metadata()?.binding().into()),
        "owner-direct" => {
            let mut raw = Vec::new();
            std::io::stdin()
                .take(codec::MAX_BYTES as u64 + 1)
                .read_to_end(&mut raw)
                .map_err(|_| Error::Io)?;
            let receipt = store.owner_receipt(&raw)?;
            Ok(receipt.source_evidence(&store)?.binding().into())
        }
        "owner" => {
            let inventory = store.inventory(Namespace::MigrationOwner)?;
            if inventory.records().len() != 1 {
                return Err(Error::Invalid);
            }
            let bytes = inventory
                .records()
                .values()
                .next()
                .ok_or(Error::Invalid)?
                .exact()?
                .ok_or(Error::Invalid)?;
            let receipt = store.owner_receipt(bytes)?;
            Ok(receipt.source_evidence(&store)?.binding().into())
        }
        _ => Err(Error::Invalid),
    }
}
fn main() {
    match run() {
        Ok(pin) => println!("{pin}"),
        Err(error) => {
            eprintln!("source-probe:{error:?}");
            std::process::exit(1)
        }
    }
}
