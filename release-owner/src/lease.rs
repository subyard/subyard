//! Exclusive compatibility-horizon mutation lease. Drop closes; it never unlocks a shared OFD.
use crate::store::Store;
use std::os::fd::OwnedFd;
pub struct UpdateLease<'a> {
    pub(crate) store: &'a Store,
    pub(crate) _lock: OwnedFd,
}
