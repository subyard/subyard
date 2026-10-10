# Retired Tauri/WebKitGTK memory measurements

Tauri/WebKitGTK was rejected for Veranda on 2026-10-09. Opening the application consumed
roughly 500–600 MiB RSS before substantial work. It is no longer a candidate or fallback,
and no further optimization or acceptance runs are planned for that stack.

These measurements are retained for comparison. They describe different historical binaries,
fixtures and rendering environments; they do not qualify a native replacement or prove the
ownership of individual allocations. RSS sums include shared pages in each process;
PSS and cgroup memory are separate measurements. The compositor and probes are excluded.

## Recorded results

The latest measured 0.1.1 Xvfb candidate used Debian 13, WebKitGTK 2.52.5, Mesa 25.0.7
and eight logical CPUs. The executable SHA-256 was
`9f97a4a7f41ef2c71a041e4cb144db45d5ef157c900546a12045f55085635b68`.
It rendered an isolated local owner with unknown yard states and no registered projects.
Thirty starts measured median 1.39 seconds and p95 1.97 seconds; peak aggregate RSS was
531 MiB and idle peak RSS was 525 MiB. Idle CPU averaged 0.28% of one core over 120 seconds.
These measurements exceeded the original 256 MiB idle memory limit. This is neither physical
fleet/mutation coverage nor acceptance of the 8 GiB Wayland baseline, other Linux hardware,
Windows or macOS. Those measurements remain historical; no further Tauri acceptance is planned.

The same GUI executable was also measured on a disposable Debian 13 Wayland VM with
four CPUs, 8 GiB nominal RAM, WebKitGTK 2.54.0, Mesa 25.0.7 and Weston 14.0.2 headless GL
at 1920×1080/60 Hz. The compositor reported llvmpipe software rendering; application
hardware acceleration was unverified. The matching 0.1.1 engine was rebuilt from the
rebased source. Thirty starts measured median 0.94 seconds and p95 2.96 seconds; startup
peak aggregate RSS was 576 MiB. After 30 seconds of settling, the 120-second idle window
measured peak RSS 591 MiB and CPU 0.025% of one core. Compositor/probe processes were
excluded; owned app/compositor cleanup and lease release passed. Startup and memory
limits were unmet under the original 256/384 MiB RSS budgets. This empty-owner control differs from the Xvfb environment and
does not establish a backend's causal effect, a leak, or prepared-fleet acceptance.

A short paired diagnostic on the same Wayland VM ran three fresh starts and ten seconds
of idle per condition. Normal idle peak RSS was 564 MiB; app-only `GDK_GL=disable`
reduced it to 471 MiB. The web process no longer had Mesa mappings in that condition,
while the main application retained them. Both measurements exceeded the original idle memory limit.
The normal-first order, warm caches and short windows limit this diagnostic; its startup
times do not replace the thirty-launch baseline. The GTK environment override remains
diagnostic-only, and application rendering settings are unchanged.

The current 0.1.1 DEB was then measured with one local owner, 20 configured uncreated yards
and 200 project names on the same Debian 13 Wayland software-rendering setup. The installed
executable SHA-256 was `440927b3a881045d22ec9ca5cb1a9bfcad51d965016951d7ba2f06247b26783d`,
verified against the exact package payload. Thirty starts measured p95 1.895 seconds and
580 MiB sampled peak RSS. After 30 seconds of settling, the 120-second idle window measured
587 MiB peak RSS, 393 MiB PSS and CPU 0.042% of one core. All four owned cleanup checks and
lease release passed. Startup and idle CPU meet their limits. At the time, idle RSS and the sampled startup
peak were below a temporary 640 MiB Linux software-rendering exception accepted on 2026-10-06.
That exception was revoked when Tauri/WebKitGTK was rejected on 2026-10-09. These results
fail the current 256 MiB idle and 384 MiB peak limits; the 128/256 MiB targets remain unmet. This establishes the configured/uncreated data workload, while initialized fleets,
switch/event peaks, stress, cycle growth and hardware acceleration were not established.

An earlier candidate, SHA-256
`3650aab56c4a2876490ddba359525837e388dfb67718a373497fa8e1ec930e41`, measured median
1.28 seconds, p95 1.37 seconds, 531 MiB peak RSS and 523 MiB idle peak RSS. One earlier
launch exited before readiness with status 101; a diagnostic reproduction and thirty
subsequent starts succeeded, with the original cause unestablished. A separate three-start,
ten-second diagnostic of that earlier binary enabled both early rendering flags and reached
the same screen with 450 MiB idle peak RSS, exceeding the original budget. These runs do not
establish a performance regression or either flag's individual effect; the historical binaries retained
their normal rendering behavior.


## Former GLib dependency disposition

The retired Linux graph used GLib 0.18.5 with a two-line upstream backport for
[RUSTSEC-2024-0429](https://rustsec.org/advisories/RUSTSEC-2024-0429.html).
The official 267679-byte crate archive had SHA-256
`233daaf6e83ae6a12a52055f568f9d7cf4671dabb78ff9560ab6da230ce00ee5`.
Only `src/variant_iter.rs` changed: `VariantStrIter::impl_get` used a mutable FFI output
pointer and passed `&mut p` instead of `&p`, following the
[upstream fix](https://github.com/gtk-rs/gtk-rs-core/commit/05dff0ee696f9bcd8617cd48c4b812d046d440cb).
The vendored source and notices were removed with the rejected desktop stack. This historical
disposition is not an exemption for any dependency in a native replacement.
