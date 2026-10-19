//! Compile-time-only physical fault control. Normal builds contain no control
//! descriptor, environment switch, or pause protocol.
#[cfg(feature = "fixture-cuts")]
mod enabled {
    use crate::{Error, Result};
    use std::{
        fs::File,
        io::{Read, Write},
        sync::{Mutex, OnceLock},
    };
    static CONTROL: OnceLock<Mutex<File>> = OnceLock::new();
    pub fn install(file: File) -> Result<()> {
        use std::os::unix::fs::FileTypeExt;
        if !file
            .metadata()
            .map_err(|_| Error::Io)?
            .file_type()
            .is_socket()
        {
            return Err(Error::Invalid);
        };
        CONTROL.set(Mutex::new(file)).map_err(|_| Error::Invalid)
    }
    pub fn point(label: &str) -> Result<()> {
        let Some(control) = CONTROL.get() else {
            return Ok(());
        };
        let mut file = control.lock().map_err(|_| Error::Io)?;
        file.write_all(label.as_bytes())
            .and_then(|_| file.write_all(b"\n"))
            .map_err(|_| Error::Io)?;
        {
            let mut polls = [rustix::event::PollFd::new(
                &*file,
                rustix::event::PollFlags::IN,
            )];
            let timeout = rustix::event::Timespec {
                tv_sec: 5,
                tv_nsec: 0,
            };
            rustix::event::poll(&mut polls, Some(&timeout)).map_err(|_| Error::Io)?;
            if polls[0].revents().is_empty() {
                return Err(Error::Io);
            };
        }
        let mut byte = [0];
        file.read_exact(&mut byte).map_err(|_| Error::Io)?;
        if byte == *b"E" {
            return Err(Error::Io);
        }
        if byte != *b"R" {
            return Err(Error::Invalid);
        };
        Ok(())
    }
}
#[cfg(feature = "fixture-cuts")]
pub use enabled::{install, point};
#[cfg(not(feature = "fixture-cuts"))]
#[inline]
pub fn point(_: &str) -> crate::Result<()> {
    Ok(())
}
