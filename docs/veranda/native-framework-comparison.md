# Native framework comparison

Slint is the Linux migration finalist after a matched comparison with Qt Widgets and GTK4.
Its tested prototype used the least memory, including the real local Go engine. The result
qualifies a pinned, patched prototype for the next implementation stage. Full application,
platform and delivery acceptance remains outstanding.

## Results

Measurements were taken on 2026-10-10. RSS and PSS are medians in MiB. Each candidate had
the same memory median, p95 and observed maximum in its visible and minimized windows.
CPU is the average percentage of one logical CPU for the entire desktop scope.

| Candidate | UI RSS | Desktop RSS | Desktop PSS | Visible CPU | Minimized CPU |
| --- | ---: | ---: | ---: | ---: | ---: |
| Slint 1.18.1, patched software renderer | 35.00 | 62.48 | 55.86 | 0.050% | 0.083% |
| Qt Widgets 6.8.2, system libraries | 51.91 | 86.93 | 76.03 | 0.050% | 0.108% |
| GTK4 4.18.6, system libraries, Cairo renderer | 41.98 | 76.72 | 62.11 | 0.008% | 0.017% |

Slint's desktop scope contained its UI and the local Go RPC process. Qt and GTK additionally
used the same native Rust bridge. All processes were counted once, including shared pages
in summed RSS. PSS was reported separately. The compositor, input/resource probes, fixture,
Incus and external workloads were outside this scope. No app-owned processes were unclassified.

Slint's local Go process used about 27.47 MiB RSS. At the final sample, its UI mappings
accounted for about 18.34 MiB of file-backed pages, 10.10 MiB of anonymous pages and
6.56 MiB of shared memory. File-backed pages include executable/library/font mappings;
anonymous pages are not an allocator-specific heap profile. This identifies broad resident
categories, without attributing individual allocations or claiming a memory effect from patches.

GTK had the lowest measured idle CPU. Qt's minimized average slightly exceeded the 0.1%
optimization target; all three stayed below the 0.5% limit. These are single bounded runs,
not statistical estimates across machines or proof of full application resource acceptance.

## Method and evidence scope

The workload was **10 yards and 100 projects in total**, with authoritative inventory/details,
all projects reported as `NOT_CREATED`, explicit Refresh and no periodic polling or event
generator. Each native UI displayed real responses from the same Rust/Go backend. Qt used a
C++ frontend and GTK a C frontend; Rust GTK/Relm4 integration costs were not measured.

The environment was Debian 13 amd64, 4 logical CPUs, 8 GiB RAM, a 40 GiB disposable VM,
labwc 0.8.3 with Pixman, no Xwayland, 1920×1080 at 60 Hz and scale 1, and an 1120×768
application window. Slint used its software Wayland renderer; Qt used its Wayland platform;
GTK used its Wayland/Cairo backend. The toolkit packages were Qt base
`6.8.2+dfsg-9+deb13u2`, Qt Wayland `6.8.2-4`, GTK `4.18.6+ds-2` and AT-SPI
`2.56.2-1+deb13u2`. Slint was built with Rust 1.92.0.

Before measurement, each exact build passed preparation, useful launch, real selection and
scrolling, Unicode input/restoration, physical Refresh, natural closure and cleanup. All seven
original checks passed, with four actual backend responses and three ordered physical pointer
acknowledgments. GUI, Go and bridge identities exited naturally, and the app cgroup and
top-level process set were empty before controlled cleanup. Fixture removal and lease release
passed. Actual useful-screen images and raw response/source bindings were reviewed.

Runs were sequential. The existing `dev/measure-veranda.py` helper measured each visible and
minimized state after 30 seconds of settling, for at least 120 seconds and 120 samples.
Process identities were stable; no process/PSS samples were missing; all windows were eligible
with no recorded pressure. App-cgroup event counters were zero. Root-cgroup event telemetry was
unavailable, so host-wide absence of pressure was not established. Memory statistics and
per-process sums were recomputed from retained raw samples.

The Slint/Qt source bundle was
`a18865f7b8c3a468935168c972eb7cda682b3f791319ffb4f60add817c22cee9`.
GTK used the corrected bundle
`dab6603a8c9788621913a9ec9012d2ee41a90469b07e52efbdcb98dff410dfca`.
Only the GTK accessibility read-error handling changed; backend, workload, environment,
method and native binaries remained identical. A failed GTK attempt stopped before resource
sampling. After a provider-node diagnostic and a successful new short check, only GTK was
remeasured. Unaffected Slint/Qt results were retained.

These bundles contain temporary prototype sources and six frozen native artifacts; they are
not a released package or a claim that the current repository HEAD builds those prototypes.
The retained source-bound evidence identifies the exact binaries and executed checks.

## Maintenance and next stage

The Slint result includes four local correctness forks: `accesskit_unix` 0.22.1,
`accesskit_atspi_common` 0.19.1, `i-slint-core` 1.18.1 and `i-slint-backend-winit` 1.18.1.
They change AT-SPI signal argument encoding, disabled accessibility states, aggregate keyboard
modifier handling across core/Winit, and complete repainting of scheduled software frames.
The pristine upstream sources were compared against the pinned sources. Stock Slint behavior
and footprint remain unqualified; no memory benefit from these patches has been established.
Migration must account for maintaining, reducing or upstreaming them and for dependency notices.
Qt/GTK system libraries were unpatched in this comparison.

The next stage is a public native shell with the full [UX contract](README.md), session/trust,
accessibility/IME and packaging coverage. The original [resource and reliability
requirements](../reference/control-plane-architecture.md#veranda-resource-budgets) remain:
ordinary 20-yard/200-project and stress 5-owner/100-yard/1000-project workloads, event bursts,
startup repetitions, 20 measured reconnects, peak memory and actual paint latency. A bounded
10-yard/100-project comparison does not satisfy those release gates. Windows/macOS and signed
delivery require their own checks. Tauri/WebKitGTK remains excluded; its [historical
measurements](webview-memory-reference.md) are retained only as reference.
