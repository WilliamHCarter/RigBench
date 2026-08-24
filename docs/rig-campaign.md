# First rig campaign — runbook

The exact sequence for the first real AR-vs-DFlash campaign, in the order the
review asked for. Nothing here has been run: no Hipfire server is reachable from
the workstation this was written on, so treat step 0 as mandatory rather than
ceremonial.

## Step 0 — prove the preparation hook by hand

`scripts/prepare-hipfire.sh` is transcribed from the review. Its `hipfire`
invocations, config keys and `/health` schema are **unconfirmed**. Run each arm
once by hand before letting the runner drive it:

```bash
./scripts/prepare-hipfire.sh ar
```

```bash
curl -s http://127.0.0.1:11435/health | jq .
```

You are looking for two things. First, that the readiness gate is real: `.model`
is `qwen3.8:27b-fast` and `.loading_model` is absent. Second, that the field
names in `configs/engines/ar.json`'s `identity_probe.record` block actually exist
in that response — `version`, `model_hash`, `draft_model` are guesses. Correct
them, or the reproducibility fields stay empty and the summary will say so.

Then the other arm, and confirm the drafter is genuinely loaded:

```bash
./scripts/prepare-hipfire.sh dflash2-f16
```

If `identity_probe.require` can be pointed at a field that distinguishes AR from
DFlash on your build — a speculation mode, a drafter name — add it. That turns
the row label from a claim into a checked fact, and the run will abort rather
than record a mislabelled row.

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
   fabricated number by design.

## Not yet in this campaign

The representative **xhigh builder profile**. Every config here is explicit
no-think, which measures the mechanical builder-performance floor. An `ar-xhigh`
/ `dflash2-xhigh` pair is closer to the intended autonomous coding workload and
should be a second sampling pair, not a change to these.

When it lands, `DecodeTokSDerived` needs revisiting rather than reusing: it is
completion tokens over the streaming window, and if hidden reasoning precedes
visible output while `completion_tokens` includes that reasoning, the figure
becomes semantically wrong. Under explicit no-think it is well-defined, which is
why it is safe for this campaign and not for the next.
