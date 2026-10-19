#![forbid(unsafe_code)]
use migrationhistory_pilot::{CompatibilityProjection, Registry, RegistryInput, StateInput};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::{
    collections::BTreeMap,
    hint::black_box,
    io::{self, Read},
    time::Instant,
};

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Request {
    cases: Vec<Case>,
    iterations: u32,
}
#[derive(Deserialize)]
#[serde(tag = "op", rename_all = "snake_case", deny_unknown_fields)]
enum Case {
    History {
        registry: RegistryInput,
        states: BTreeMap<String, StateInput>,
        traces: Vec<Vec<String>>,
    },
    Registry {
        registry: RegistryInput,
    },
    Binding {
        registry: RegistryInput,
        other: RegistryInput,
    },
    Projection {
        epochs: BTreeMap<String, i64>,
        #[serde(default)]
        pin: String,
    },
}
#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct Response {
    results: Vec<Value>,
    elapsed_nanos: u128,
    iterations: u32,
}
// Conservative adapter limits, checked on DTOs before constructing checked values
// or semantic/JSON result trees. Saturation always exceeds a finite budget.
const MAX_WORK: u64 = 2_000_000_000;
const MAX_RESULT_BYTES: u64 = 4 * 1024 * 1024;
const MAX_CELLS: u64 = 100_000;

fn registry_weight(r: &RegistryInput) -> u64 {
    let mut size = 1u64;
    for (name, _) in r.minimum.iter().chain(&r.current) {
        size = size.saturating_add(1 + name.len() as u64);
    }
    for (name, pin) in &r.retired {
        size = size.saturating_add(1 + name.len() as u64 + pin.len() as u64);
    }
    for m in &r.migrations {
        size = size
            .saturating_add(1 + m.id.len() as u64 + m.domain.len() as u64 + m.kind.len() as u64);
        for dep in &m.depends_on {
            size = size.saturating_add(1 + dep.len() as u64);
        }
    }
    size
}
fn check_budget(request: &Request) -> Result<(), &'static str> {
    let (mut work, mut result_bytes, mut retained_cells, mut live_cells) =
        (0u64, 128u64, 0u64, 0u64);
    // JSON escaping uses at most six bytes per input byte. Fixed allowances
    // include punctuation, scalar epochs and object/array fields, not RSS.
    let string_bytes = |s: &str| (s.len() as u64).saturating_mul(6).saturating_add(4);
    for case in &request.cases {
        match case {
            Case::History {
                registry,
                states,
                traces,
            } => {
                let domains = registry.minimum.len() as u64;
                let migrations = registry.migrations.len() as u64;
                let frontier_cells = domains.saturating_add(migrations).saturating_add(1);
                let mut snapshot = 16u64;
                for name in registry.minimum.keys() {
                    snapshot = snapshot
                        .saturating_add(string_bytes(name))
                        .saturating_add(64);
                }
                let mut ids_bytes = 0u64;
                for m in &registry.migrations {
                    ids_bytes = ids_bytes.saturating_add(string_bytes(&m.id));
                }
                snapshot = snapshot.saturating_add(ids_bytes);
                // Pending IDs, canonical next and its after-frontier.
                let mut bytes = 128u64
                    .saturating_add(ids_bytes)
                    .saturating_add(snapshot)
                    .saturating_add(ids_bytes);
                let mut cells = frontier_cells;
                let mut steps = 4u64;
                let mut max_trace = 0u64;
                for trace in traces {
                    let length = trace.len() as u64;
                    max_trace = max_trace.max(length);
                    steps = steps.saturating_add(length.saturating_mul(4).saturating_add(2));
                    bytes = bytes
                        .saturating_add(snapshot.saturating_mul(2))
                        .saturating_add(64);
                    for id in trace {
                        bytes = bytes.saturating_add(string_bytes(id));
                    }
                    cells = cells
                        .saturating_add(frontier_cells.saturating_mul(2))
                        .saturating_add(length)
                        .saturating_add(1);
                }
                let mut state_weight = states.len() as u64;
                for (name, state) in states {
                    state_weight = state_weight.saturating_add(name.len() as u64);
                    for id in &state.applied {
                        state_weight = state_weight.saturating_add(1 + id.len() as u64);
                    }
                }
                // A history check scans the migration list for every domain.
                // Each trace may rewind/advance, clone and validate frontiers.
                let step_work = registry_weight(registry)
                    .saturating_mul(domains.saturating_add(1))
                    .saturating_add(state_weight);
                work = work.saturating_add(step_work.saturating_mul(steps));
                result_bytes = result_bytes.saturating_add(bytes);
                retained_cells = retained_cells.saturating_add(cells);
                let edge_cells = registry
                    .migrations
                    .iter()
                    .map(|m| (m.depends_on.len() as u64).saturating_add(4))
                    .max()
                    .unwrap_or(0);
                live_cells = live_cells.max(
                    frontier_cells
                        .saturating_mul(max_trace.saturating_mul(2).saturating_add(5))
                        .saturating_add(max_trace.saturating_mul(edge_cells)),
                );
            }
            Case::Registry { registry } => {
                work = work.saturating_add(registry_weight(registry));
                result_bytes = result_bytes.saturating_add(64);
            }
            Case::Binding { registry, other } => {
                let width = (registry.minimum.len() as u64)
                    .saturating_add(registry.migrations.len() as u64)
                    .saturating_add(1);
                work = work.saturating_add(
                    registry_weight(registry)
                        .saturating_add(registry_weight(other))
                        .saturating_mul(width)
                        .saturating_mul(width)
                        .saturating_mul(8),
                );
                live_cells = live_cells.max(width.saturating_mul(width).saturating_mul(4));
                result_bytes = result_bytes.saturating_add(128);
            }
            Case::Projection { epochs, pin } => {
                let size = epochs.keys().fold(pin.len() as u64, |n, name| {
                    n.saturating_add(1 + name.len() as u64)
                });
                work = work.saturating_add(size);
                result_bytes = result_bytes.saturating_add(64);
            }
        }
    }
    if result_bytes > MAX_RESULT_BYTES || retained_cells.saturating_add(live_cells) > MAX_CELLS {
        return Err("result-budget");
    }
    if work.saturating_mul(u64::from(request.iterations).saturating_add(1)) > MAX_WORK {
        return Err("work-budget");
    }
    Ok(())
}

fn evaluate(case: &Case) -> Value {
    match case {
        Case::Registry { registry } => json!({"accepted": Registry::new(registry.clone()).is_ok()}),
        Case::Projection { epochs, pin } => {
            json!({"accepted": CompatibilityProjection::new(epochs.clone(), pin.clone()).is_ok()})
        }
        Case::Binding { registry, other } => {
            let (Ok(r), Ok(other)) = (
                Registry::new(registry.clone()),
                Registry::new(other.clone()),
            ) else {
                return json!({"accepted":false});
            };
            let Ok(edge) = r.next(&r.baseline()) else {
                return json!({"accepted":false});
            };
            let base = r.baseline();
            let ids = r
                .pending(&base)
                .unwrap()
                .iter()
                .map(|m| m.id.clone())
                .collect::<Vec<_>>();
            let trace = r.trace_from(&base, &ids).unwrap();
            let completion = r.complete(trace.terminal()).unwrap();
            json!({"accepted":other.check_edge_binding(&edge).is_ok(), "identity":other.check_edge(&edge).is_ok(), "completionIdentity":other.check_completion(&completion).is_ok()})
        }
        Case::History {
            registry,
            states,
            traces,
        } => {
            let Ok(r) = Registry::new(registry.clone()) else {
                return json!({"accepted":false});
            };
            let Ok(a) = r.history(states.clone()) else {
                return json!({"accepted":false});
            };
            let pending: Vec<_> = r.pending(&a).unwrap().iter().map(|m| &m.id).collect();
            let next = match r.next(&a) {
                Ok(e) => json!({"id":e.migration().id,"after":e.after().domains()}),
                Err(_) => Value::Null,
            };
            let traces: Vec<_> = traces.iter().map(|ids| match r.trace_from(&a, ids) {
                Ok(t) => json!({"accepted":true, "initial":t.initial().domains(), "terminal":t.terminal().domains(), "edges":t.edges().iter().map(|e| &e.migration().id).collect::<Vec<_>>()}),
                Err(_) => json!({"accepted":false}),
            }).collect();
            json!({"accepted":true,"pending":pending,"next":next,"complete":r.complete(&a).is_ok(),"traces":traces})
        }
    }
}
fn run() -> Result<(), &'static str> {
    let mut bytes = vec![];
    io::stdin()
        .take(1_048_577)
        .read_to_end(&mut bytes)
        .map_err(|_| "input")?;
    if bytes.len() > 1_048_576 {
        return Err("oversized");
    }
    let request: Request = serde_json::from_slice(&bytes).map_err(|_| "document")?;
    if request.iterations == 0 || request.iterations > 10_000 || request.cases.len() > 10_000 {
        return Err("bounds");
    }
    check_budget(&request)?;
    let start = Instant::now();
    for _ in 0..request.iterations {
        for case in &request.cases {
            black_box(evaluate(black_box(case)));
        }
    }
    let elapsed_nanos = start.elapsed().as_nanos();
    let results = request.cases.iter().map(evaluate).collect();
    serde_json::to_writer(
        io::stdout(),
        &Response {
            results,
            elapsed_nanos,
            iterations: request.iterations,
        },
    )
    .map_err(|_| "output")
}
fn main() {
    if let Err(code) = run() {
        eprintln!("migrationhistory-pilot: {code}");
        std::process::exit(1);
    }
}
