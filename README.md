# AgentBench-01

An engine-agnostic benchmark that predicts the wall-clock speed and quality of a
PM → builder → reviewer coding workflow, and is used to select the fastest local
inference configuration that preserves implementation and review quality.

**North-star metric: seconds per successful Story.** Token throughput, TTFT,
prefix-cache reuse, speculation acceptance and GPU utilization are explanatory.
They are not the optimization target, and no configuration is "better" unless it
wins on the benchmark while meeting the benchmark's quality gates.

## Status

| Slice | State |
|---|---|
| v0.1 — single-call runner + cold builder fixture | complete, acceptance gate green |
| v0.2 — multi-turn builder replay + prefix layout | complete, acceptance gate green |
| v0.2.1 — review fixes: executable engine identity | complete |
| v0.2.2 — review fixes: run schedule topology | complete |
| v0.2.3 — gate stack controls + toolchain pin | complete |
| v0.3 — live builder repair loop | complete, acceptance gate green |
| v0.3.1 — extensibility addendum (knobs, cache, provenance) | complete |
| v0.3.2 — adversarial review of the live loop | complete |
| v0.3 and later | not started |

Ready for a first rig campaign — see [docs/rig-campaign.md](docs/rig-campaign.md).
Step 0 of that runbook is mandatory: the Hipfire preparation script is
transcribed from review and unverified from this workstation.

## Quick start

Prove the fixture's controls fire before trusting any result:

```bash
go run ./cmd/agentbench verify-fixture
```

Run the end-to-end acceptance gates against the in-repo mock endpoint:

```bash
go run ./cmd/agentbench smoke
```

Just one slice's gate:

```bash
go run ./cmd/agentbench smoke -slice v0.2
```

Run against a real endpoint. `-before-engine` is not optional for an A/B: an
OpenAI-compatible request cannot select a speculation mode, so without it two
configs sent to one daemon produce two differently *labelled* rows from one
actual configuration.

```bash
go run ./cmd/agentbench run -before-engine ./scripts/prepare-hipfire.sh -engines configs/engines/ar.json,configs/engines/dflash2.json -endpoint http://127.0.0.1:11435/v1 -thermal steady -warmup 1 -repeats 3
```

Run the live edit/test/repair loop, which writes `story.json`:

```bash
go run ./cmd/agentbench run -lane builder-live -thermal steady -repeats 1
```

Replay the four-turn builder trajectory instead of a single turn:

```bash
go run ./cmd/agentbench run -turns -layout configs/layouts/builder-cache-friendly.json -thermal cold
```

## Layout

```
cmd/agentbench/              runner CLI
internal/
  client/                    OpenAI-compatible streaming client + telemetry adapters
  config/                    fixture, engine, layout and context-pack loaders
  executor/                  worktree staging, patch apply, rungs, mutant controls
  metrics/                   per-request record schema, JSONL writer, run identity
  mock/                      deterministic in-repo endpoint and canned candidates
  prompt/                    deterministic serializer, hashing, layout resolution
  report/                    aggregation, summary.md, summary.csv
  runner/                    lane orchestration
  scoring/                   builder quality gates
fixtures/zig-playback-v1/    the frozen synthetic Zig fixture
configs/engines/             AR, DFlash2, and their mock counterparts
configs/layouts/             current vs cache-friendly prompt layouts
runs/<run-id>/               volatile output; never embedded into a prompt
```

## What the benchmark measures, and what it refuses to

- **Wall clock to a quality-gated passing patch.** A run that fails a gate keeps
  its timing — the measurement is real — but is not eligible to be a champion.
- **Gates are three-valued.** Pass, fail, and *skipped*. A rung that could not
  run (a missing toolchain, a build that never happened) is never scored as a
  rung that passed.
- **Telemetry is nullable and never fabricated.** A field the engine did not
  report is `null`, and the report prints `not exposed` — which is not zero.
- **Cold, first-capture and steady rows never aggregate together**, and the
  thermal class is stated by the operator, never inferred from elapsed time.
- **Rows that are not comparable are not merged.** Different prompt hashes,
  model hashes, engine commits or reasoning budgets in one cell are called out
  in the summary rather than averaged.
- **Prompt layout is a benchmark input.** Stable bytes precede volatile ones,
  later turns append rather than rewrite, and the reusable-prefix digest is
  verified to be a digest of a real byte prefix. A layout A/B is required to be a
  pure reordering of identical bytes, so a "cache-friendly" layout cannot win by
  quietly rewording the prompt.
- **Engine identity is produced and checked, not asserted.** A preparation hook
  owns server lifecycle and an identity probe verifies it; rows from a run with
  neither are marked *asserted* and the summary says the labels may all describe
  one configuration.
- **`thinking: off` must say how.** Omitting a reasoning field does not disable
  reasoning, so a config claiming no-think has to name the mechanism, and the
  acceptance gate checks the sent bytes against a negative control.
- **The benchmark never shows the model its own oracle.** In a live lane the
  hidden suite is injected once, after the loop stops. The acceptance gate scans
  every prompt of every turn for hidden-suite content, with the diagnostic lane
  as its negative control.
- **Engine performance and workload outcome are separate groups.** A
  configuration that decodes faster, generates twice as much, and fails the task
  cannot look competitive: `story.inference` and `story.task` never blend.
- **Three numbers get called "the cache hit" and none is dropped.** What the
  prompt makes reusable, what a turn shares with the one before it, and what the
  engine says it reused are recorded separately, per turn.
- **Tuning axes are data, not code.** 25 named knobs reach the preparation hook
  as environment variables; the benchmark records what was asked for and has no
  opinion about which values are good.
- **The north star is seconds per hidden-green Story**, and medians cover green
  stories only — a story that failed fast is not a fast story.
- **The gate stack has controls of its own.** A patch that adds one comment
  line compiles and passes the visible suite, because that suite only tests
  pre-seam behaviour. `candidate_tests_discriminate` replays the candidate's own
  tests against the frozen HEAD and requires them to go red, and `unwired` /
  `comment-only` are permanent acceptance rows.
- **The compiler is part of the fixture.** `toolchain.zig` is checked, not
  recorded: a fixture verified under one Zig and run under another is refused.
- **Engines are never interleaved across repetitions.** Preparation is engine
  outer, repetition inner, and warmup is once per engine — otherwise a
  `-repeats 3` A/B measures the later repeats against whichever engine was
  prepared last. The schedule is asserted before the first request is sent.
- **A replay turn carries no verdict.** A four-turn replay judges its scored turn
  and records the rest as context-growth measurements. They are never counted as
  failures.

## The fixture proves itself before it measures anything

`zig-playback-v1` is a synthetic Zig repository derived from the shape of a real
playback seam — half-open frame ranges, a per-voice tone override, a 72-byte
audio-plane record whose layout must not move, and a per-frame path with a
structural rule about where a coefficient may be computed.

Two controls run before any result is trusted:

- **Tripwires.** A deliberately failing test is appended to each hidden source
  file in turn; each must turn the rung red, or that file is compiled but never
  executed.
- **Anti-vacuity mutants.** Twelve planted defects are applied to the reference
  solution. Each must be caught. Three of them — a `Tone` field smuggled into
  the hot record, a coefficient resolved inside the frame loop, and a retained
  pointer — **pass the visible test suite** and are caught only by the hidden
  suite. That is the hidden suite earning its place rather than restating the
  visible one.

## What the layout A/B actually shows

Within one story both layouts append cleanly: a volatile-first layout's leading
objective does not change between turns of the same task, so turn-to-turn reuse
is not where they differ. The difference is **across tasks** — what a second
story could reuse from the first:

| Layout | Reusable prefix | Share of prompt | Cross-task reuse |
|---|---|---|---|
| `builder-cache-friendly` | 27,191 B | 69% | 27,136 B (98%) |
| `builder-current` | 1,552 B | 4% | 1,536 B (6%) |

Slice records: [docs/v0.1.md](docs/v0.1.md), [docs/v0.2.md](docs/v0.2.md),
[docs/v0.2.1.md](docs/v0.2.1.md), [docs/v0.2.2.md](docs/v0.2.2.md),
[docs/v0.2.3.md](docs/v0.2.3.md), [docs/v0.3.md](docs/v0.3.md), [docs/v0.3.1.md](docs/v0.3.1.md), [docs/v0.3.2.md](docs/v0.3.2.md).

## Toolchain

- Go 1.26 for the runner.
- Zig via [anyzig](https://github.com/marler8997/anyzig): the compiler version is
  resolved from the nearest `build.zig.zon`, so
  `fixtures/zig-playback-v1/repo/build.zig.zon` is the single pin and CI needs no
  separate version file. `zig version` outside a project will fail; that is
  expected.
- `git` for worktree staging and patch application.
