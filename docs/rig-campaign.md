# First rig campaign — runbook

The exact sequence for the first real AR-vs-DFlash campaign, in the order the
review asked for. Nothing here has been run: no Hipfire server is reachable from
the workstation this was written on, so treat step 0 as mandatory rather than
ceremonial.

## What this rig is, as established

Recorded here so nobody re-derives it from a terminal session.

| | Value |
|---|---|
| serve / request alias | `qwen3.8:27b-fast` |
| `/health.model` (resolved) | `qwen3.8:27b-mq4-xt` |
| target | `/home/willy/.hipfire/models/qwen3.8-27b.mq4r` |
| target sha256 | `61072980798ac1d3325020a63171d1a9cf99103eaa5bb1675a37845ea7d7762e` |
| draft | `/home/willy/.hipfire/models/qwen3.8-27b-dflash2-f16.hfq` |
| draft sha256 | `d466f5b611ca907d61e5d45745a659397244a188e66dfa42341fcc2a6ea8c112` |
| endpoint | `http://127.0.0.1:11435/v1` |

The serve alias and the health identity are **different strings**. Defaulting the
readiness check to the serve alias produced a five-minute false timeout, so the
hook now defaults `AGENTBENCH_HEALTH_MODEL` to the resolved value and the rig
configs assert it in `identity_probe.require`.

Hipfire's health payload does **not** expose speculation state, so the probe
attests the resolved target only. Which arm is running is attested by the
preparation log's config dump.

## Step 0 — prove the preparation hook by hand

`scripts/prepare-hipfire.sh` is transcribed from the review. Its `hipfire`
invocations, config keys and `/health` schema are **unconfirmed**. Run each arm
once by hand before letting the runner drive it:

First, validate the knob mapping without touching the server. This needs no
Hipfire toolchain and takes a second:

```bash
./scripts/prepare-hipfire.sh ar --check-knobs && ./scripts/prepare-hipfire.sh dflash2-f16 --check-knobs
```

It exits non-zero on any axis this hook has no **verified** key for. Three are
deliberately refused rather than guessed — `speculative_block`, `verify_mode`,
`prompt_cache_mode` — because mapping a label to a guessed key records a setting
the server never had.

Then the real thing:

```bash
./scripts/prepare-hipfire.sh ar
```

```bash
curl -s http://127.0.0.1:11435/health | jq .
```

Confirm `.model` is `qwen3.8:27b-mq4-xt` and `.loading_model` is absent. Then
check the field names in `identity_probe.record` — `version`, `model_hash`,
`draft_model` — against that response. They are still guesses; correct them or
the reproducibility fields stay empty and the summary will say so.

Then the other arm, and confirm the drafter is genuinely loaded:

```bash
./scripts/prepare-hipfire.sh dflash2-f16
```

If the health payload gains a field that distinguishes AR from DFlash, add it to
`identity_probe.require`. That turns the row label from a claim into a checked
fact, and the run aborts rather than recording a mislabelled row.

### What the hook now handles for you

- **Stale daemon reaping.** `hipfire stop` has been seen to kill the foreground
  serve process while leaving `~/.hipfire/bin/daemon` alive, so the next launch
  fails with "daemon already running". The hook reaps only that exact managed
  binary, escalates to SIGKILL, clears stale pidfiles, and **fails closed** if
  the port is still held by a process it did not start. This matters more than
  it used to: the live-lane repeat protocol relies on re-preparation between
  stories, so an intermittently-failing restart silently breaks that guarantee.
- **PM4 verification is launch-time env**, not a config key. Setting
  `knobs.pm4_verify` exports `HIPFIRE_DFLASH_VERIFY_PM4=1` for the serve
  invocation. Not part of the baseline campaign.

## Step 1 — local gates first

```bash
go run ./cmd/agentbench verify-fixture
```

```bash
go run ./cmd/agentbench smoke
```

Both must be green before any rig time is spent. `verify-fixture` proves the
hidden suite executes and that all twelve anti-vacuity controls fire; `smoke`
proves every quality gate is individually reachable, that the layout A/B is a
pure reordering, and that `thinking: off` produces a request which actually
disables thinking.

## Step 2 — first-capture single-turn A/B

One request per engine. Cold, capture costs included and visible.

```bash
go run ./cmd/agentbench run -before-engine ./scripts/prepare-hipfire.sh -engines configs/engines/ar.json,configs/engines/dflash2.json -endpoint http://127.0.0.1:11435/v1 -thermal first-capture -verify-fixture
```

## Step 3 — steady-state single-turn A/B

One discarded warmup, three repetitions. This is the clean equivalent of the
33.96 vs 105.08 tok/s microbench, but now with a real builder patch and the
hidden-test quality gate attached.

The runner schedules this as **engine outer, repetition inner**:

```
prepare AR       warm AR once     AR rep 1   AR rep 2   AR rep 3
prepare DFlash   warm DFlash once DFlash rep 1  DFlash rep 2  DFlash rep 3
```

Engines are never interleaved across repetitions, and the warmup is one per
engine rather than one per measurement. Both properties are asserted before the
first request is sent, and pinned by tests in `cmd/agentbench/schedule_test.go`.

```bash
go run ./cmd/agentbench run -before-engine ./scripts/prepare-hipfire.sh -engines configs/engines/ar.json,configs/engines/dflash2.json -endpoint http://127.0.0.1:11435/v1 -thermal steady -warmup 1 -repeats 3
```

## Step 3b — the FIRST live probe: one story each, inspected by hand

Before any statistical run. The point is to read two trajectories, not to
collect a median.

```bash
go run ./cmd/agentbench run -lane builder-live -before-engine ./scripts/prepare-hipfire.sh -engines configs/engines/ar-medium-live.json -endpoint http://127.0.0.1:11435/v1 -thermal steady -repeats 1 -server-log ~/.hipfire/serve.log
```

```bash
go run ./cmd/agentbench run -lane builder-live -before-engine ./scripts/prepare-hipfire.sh -engines configs/engines/dflash2-f16-medium-live.json -endpoint http://127.0.0.1:11435/v1 -thermal steady -repeats 1 -server-log ~/.hipfire/serve.log
```

**Medium reasoning, not no-think.** The no-think configs measured the mechanical
floor and neither completed a patch; the medium-reasoning AR run is the one that
reached a compiling tree, which is why the live loop exists. The no-think configs
are kept unchanged so the closed one-shot experiment stays reproducible.

**Run AR first, read it, then run DFlash.** Not both in one invocation. The point
of this step is two trajectories a human has looked at, and a failure in the
first tells you whether the second is worth the time.

**No `context_tokens` on these configs.** The baseline is not a context
experiment, and pinning an arbitrary capacity adds a variable to an AR-vs-DFlash
comparison nobody asked a question about. When context is swept deliberately, the
axis maps to Hipfire's `max_seq`.

**One repetition.** Repeated live stories replay a byte-identical turn 0 against
a server whose prompt cache the previous repetition filled, so a three-repeat
median can become one story plus two cached replays. The runner refuses
`-repeats > 1` on a live lane without `-before-engine`, and re-prepares between
repetitions when there is one — but establish that a trajectory succeeds at all
before spending time on statistics.

**No `-warmup`.** It is refused on a live lane. Priming with the one-shot prompt
measures a different workload; priming with the live prompt fills the cache with
the story about to be measured. Residency comes from the preparation hook.

Read `story.json` first, then the per-turn prompts and diffs under
`artifacts/<slug>/t*/`. What to look for:

- did turn 0 produce a diff at all, and did it apply?
- what does `turn_role` / `turn_max_tokens` / `finish_reason` say — is the model
  hitting the per-turn ceiling, or stopping naturally?
- does `time_to_discriminating_green` ever get set, or does the loop end at
  `turn budget exhausted`?
- `cache` per turn: is the engine reporting reuse, and does it track the
  runner's own `shared_with_previous_tokens`?

## Step 4 — the four-turn trajectory, cache-friendly layout

The first genuinely relevant result: total builder trajectory time, turn-by-turn
TTFT, real cache behaviour, and final-patch correctness.

```bash
go run ./cmd/agentbench run -before-engine ./scripts/prepare-hipfire.sh -engines configs/engines/ar.json,configs/engines/dflash2.json -endpoint http://127.0.0.1:11435/v1 -layout configs/layouts/builder-cache-friendly.json -turns -thermal steady -warmup 1 -repeats 3
```

## Step 5 — the same trajectory under the current layout

```bash
go run ./cmd/agentbench run -before-engine ./scripts/prepare-hipfire.sh -engines configs/engines/ar.json,configs/engines/dflash2.json -endpoint http://127.0.0.1:11435/v1 -layout configs/layouts/builder-current.json -turns -thermal steady -warmup 1 -repeats 3
```

Expect the interesting result here to be the **engine's own cache telemetry**,
not a dramatic same-story win. Both layouts append cleanly within one story, so
the reusable-prefix difference this measures is the cross-story one — see
[v0.2](v0.2.md).

## What to hand back

Per run directory:

```
runs/<run-id>/request.jsonl
runs/<run-id>/run.json
runs/<run-id>/summary.md
runs/<run-id>/summary.csv
runs/<run-id>/artifacts/engine-prep/*.prepare.log
runs/<run-id>/artifacts/engine-prep/*.probe.json
runs/<run-id>/artifacts/<engine>.<layout>.<pack>.<thermal>.t*/request.json
runs/<run-id>/artifacts/<engine>.<layout>.<pack>.<thermal>.t*/candidate.patch
runs/<run-id>/artifacts/<engine>.<layout>.<pack>.<thermal>.t*/hidden-tests.log
```

The two `engine-prep` files are what make the row labels defensible: the
preparation log shows the AR arm was configured with speculation off, and the
probe response shows what the daemon reported at the moment it was prepared.

## Read the summary in this order

1. **§1 quality gates.** A configuration that is faster and misses a hidden test
   is not a champion.
2. **§4 engine identity.** Any row marked **asserted** rather than attested was
   labelled from a config file. Stop there and fix the hook before reading on.
   Then check `run.json`'s `warmup_policy`: it states how many priming requests
   were sent and whether engines were interleaved, so the schedule that produced
   the rows is recorded and not something you have to remember.
3. **§2 wall clock**, cold and warm never averaged together.
4. **§3 telemetry.** `not exposed` is not zero. If the Hipfire columns are all
   `not exposed`, the adapter's provisional key names are wrong — the fix is in
   `internal/client/telemetry.go`, and a wrong guess yields null rather than a
   fabricated number by design. Pass `-server-log ~/.hipfire/serve.log` to pick
   up what the server writes there instead of the response stream; the parser
   reads both `key=value` pairs and the parenthetical
   `(32768 tok, 5593 windows)` form.

## Not yet in this campaign

**Steps 2 and 3 are explicit no-think** and measure the mechanical floor; step 3b
onward is medium. They are different sampling variants and their rows never
aggregate — `thinking` is part of every record and the reporter refuses to merge
across it.

The representative **xhigh builder profile** is a third pair, `ar-xhigh` /
`dflash2-f16-xhigh`, added as new configs rather than as edits to these. The hook
already accepts those profile names.

**No engine tuning.** F16 DFlash unchanged; no MQ6/MQ4 drafts, no adaptive block,
no PM4, no KV changes, until a successful live trajectory exists to optimize
against.

When it lands, `DecodeTokSDerived` needs revisiting rather than reusing: it is
completion tokens over the streaming window, and if hidden reasoning precedes
visible output while `completion_tokens` includes that reasoning, the figure
becomes semantically wrong. Under explicit no-think it is well-defined, which is
why it is safe for this campaign and not for the next.
