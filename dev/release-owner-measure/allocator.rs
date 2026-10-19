// Developer-only snapshot instrumentation. Never linked into production packages.
mod allocation_measure {
    use std::alloc::{GlobalAlloc, Layout, System};
    use std::sync::atomic::{AtomicU64, Ordering::Relaxed};
    pub struct Counted;
    static CALLS: AtomicU64 = AtomicU64::new(0);
    static BYTES: AtomicU64 = AtomicU64::new(0);
    static REALLOCS: AtomicU64 = AtomicU64::new(0);
    static FREES: AtomicU64 = AtomicU64::new(0);
    std::thread_local! { static SKIP: std::cell::Cell<bool> = const { std::cell::Cell::new(false) }; }
    fn count(bytes: usize, realloc: bool) {
        if SKIP.try_with(|skip| !skip.get()).unwrap_or(false) {
            CALLS.fetch_add(1, Relaxed);
            BYTES.fetch_add(bytes as u64, Relaxed);
            if realloc { REALLOCS.fetch_add(1, Relaxed); }
        }
    }
    unsafe impl GlobalAlloc for Counted {
        unsafe fn alloc(&self, layout: Layout) -> *mut u8 {
            let result = unsafe { System.alloc(layout) };
            if !result.is_null() { count(layout.size(), false); }
            result
        }
        unsafe fn alloc_zeroed(&self, layout: Layout) -> *mut u8 {
            let result = unsafe { System.alloc_zeroed(layout) };
            if !result.is_null() { count(layout.size(), false); }
            result
        }
        unsafe fn realloc(&self, ptr: *mut u8, layout: Layout, size: usize) -> *mut u8 {
            let result = unsafe { System.realloc(ptr, layout, size) };
            if !result.is_null() { count(size, true); }
            result
        }
        unsafe fn dealloc(&self, ptr: *mut u8, layout: Layout) {
            if SKIP.try_with(|skip| !skip.get()).unwrap_or(false) { FREES.fetch_add(1, Relaxed); }
            unsafe { System.dealloc(ptr, layout) }
        }
    }
    pub fn emit(final_report: bool) {
        // Suppress this thread's reporting allocations, not application threads.
        SKIP.with(|skip| skip.set(true));
        if let Some(directory) = std::env::var_os("SUBYARD_MEASURE_STATS_DIR") {
            let report = format!("{{\"schema_version\":1,\"runtime\":\"rust\",\"pid\":{},\"final\":{},\"successful_allocation_calls_including_realloc\":{},\"requested_allocation_bytes_including_full_realloc_size\":{},\"reallocation_calls\":{},\"deallocation_calls\":{}}}\n",
                std::process::id(), final_report, CALLS.load(Relaxed), BYTES.load(Relaxed), REALLOCS.load(Relaxed), FREES.load(Relaxed));
            let target = std::path::Path::new(&directory).join(format!("rust-{}.json", std::process::id()));
            let pending = target.with_extension("pending");
            if std::fs::write(&pending, report).is_ok() { let _ = std::fs::rename(pending, target); }
        }
        SKIP.with(|skip| skip.set(false));
    }
    pub fn exit(code: i32) -> ! { emit(true); std::process::exit(code) }
}
#[global_allocator]
static MEASURE_ALLOCATOR: allocation_measure::Counted = allocation_measure::Counted;
