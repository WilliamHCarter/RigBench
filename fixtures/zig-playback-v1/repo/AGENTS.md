# Repository doctrine — zig-playback

Read this before editing. These rules are older than any single task and a task
that contradicts one of them is wrong about the repository, not the other way
round.

## Build rungs

```
zig build test           visible suite, Debug
zig build test-release   the whole graph rebuilt in ReleaseFast
zig build test-hidden    whatever hidden/root.zig contains
```

`build.zig`, `build.zig.zon` and `hidden/` are not owned by bounded tasks. A
rung you did not run is not a rung you may report.

## Hot and cold

`voice.Hot` is the audio-plane record. It is `extern`, exactly 72 bytes, and its
field offsets are pinned by tests. Setup-time data belongs in `voice.Cold`. If a
seam appears to need a new `Hot` field, the seam is wrong: resolve the value
once at note-on into a field that already exists, or pass it into the render call.

`Cold` retains no pointer of any kind. A voice that held a document, parameter
or bank pointer could read it after the control plane replaced it.

## The per-frame path

Order is fixed: reader x envelope x voice gain, then the optional one-pole
filter, then declick. Nothing else may be inserted between them.

Inside the frame loop there is no allocation, no logging, no locking, no
transcendental, no `@import`, and no read of cold state. Anything derived from
setup state is computed once before the loop. This is a structural rule and it
is checked structurally, because ordinary tests cannot observe where a
computation happened.

## Refusals are not clamps

Invalid input is refused **by name**, through an error union, and the refused
operation mutates nothing. Do not invent permissive behaviour: a clamped range,
a canonicalized NaN or a saturated coefficient is a defect even when the output
looks reasonable. In particular `Cursor.advance` must decide whether the next
source access is legal **before** it writes any cursor state.

## Bounds are half-open

Every frame interval in this repository is `[start, end)`. Forward playback
stops before reading `end`. Reverse playback never reads below `start`.

## Goldens

`products/player/golden.zig` is frozen. If your change moves those bytes, your
change is not default-compatible. Blessing the golden is never the repair: stop
and report the first differing value and your diagnosis.

## Tests

Name a test after the invariant it defends, not after the function it calls. For
each test, ask: **would this fail if the body under test were emptied?** Where
the answer is no, say so in your report rather than counting it as coverage.
