# S2 — the slice-playback seam: `FrameRange` and `Tone`

Bounded implementation slice. You own the voice-engine seam and nothing else.

## Scope

You own, and may change only:

```
src/voice/range.zig       (new)
src/voice/tone.zig        (new)
src/voice/cursor.zig
src/voice/engine.zig
src/voice/root.zig
src/voice/range_test.zig  (new)
src/voice/tone_test.zig   (new)
src/voice/cursor_test.zig
src/voice/engine_test.zig
```

Everything else is out of scope, explicitly including `build.zig`,
`build.zig.zon`, `src/core/**`, `products/**` and `hidden/**`. Do not expand into
choke, steal, chop operations, BPM sync, presets, UI, a renderer schema, or any
later story's modulation. Do not add dependencies. Do not edit or bless
`products/player/golden.zig`.

## Interfaces to implement

Use these exact names and shapes unless a file already committed in this
repository contradicts them, in which case report the contradiction before
choosing a different design.

```zig
// src/voice/range.zig
pub const FrameRange = struct {
    start: u64,
    end: u64,
    pub fn full(frames: u64) FrameRange;
    pub fn validate(self: FrameRange, frames: u64) bool;
    pub fn len(self: FrameRange) u64;
};

// src/voice/tone.zig
pub const ToneError = error{ BadGain, BadCoefficient };
pub const Filter = union(enum) {
    inherit_segment,
    bypass,
    coefficient: f32,
};
pub const Tone = struct {
    gain: f32 = 1.0,
    filter: Filter = .inherit_segment,
    pub fn resolveCoeff(self: Tone, segment_coeff: f32) ToneError!f32;
};
```

`voice.Cold` gains exactly two fields, both defaulted so that existing
construction sites keep their current meaning:

```zig
range: ?FrameRange = null,   // null means the whole sample
tone: Tone = .{},
```

`Cursor.advance`, `Cursor.taps` and `Cursor.isCoherent` take a `FrameRange`
instead of a loose `lo, hi` pair. The cursor owns the half-open advance.

`src/voice/root.zig` follows the existing idiom -- every module re-exported and
referenced inside `test {}` -- and must expose the new surface under these exact
names, because consumers outside `src/voice/` reach the seam through the module
root and not through file paths:

```zig
pub const range = @import("range.zig");
pub const tone  = @import("tone.zig");
pub const FrameRange = range.FrameRange;
pub const Tone       = tone.Tone;
pub const Filter     = tone.Filter;
pub const ToneError  = tone.ToneError;
```

## Invariants

1. `FrameRange` is half-open `[start, end)`. A range is valid iff
   `start < end` and `end <= frames`. An invalid range is refused by name at
   note-on as `error.InvalidRange`; it is never clamped and never silently
   widened to the full sample.
2. `FrameRange.full(frames)` is `.{ .start = 0, .end = frames }`.
3. A `Cold` with `range = null` and `tone = .{}` renders **byte-identically** to
   the behaviour before this seam existed. `products/player/golden.zig` is the
   witness and it is frozen.
4. `Hot` remains `extern`, exactly 72 bytes, with every existing field at its
   existing offset. `FrameRange` and `Tone` are cold state.
5. `Cold` still retains no pointer.
6. Forward playback stops before reading `range.end`. Reverse playback never
   reads below `range.start`. Reverse playback starts at `range.end - 1`.
7. `Cursor.advance` decides whether the next source access is legal before it
   commits any cursor state. A refused advance leaves `frame`, `phase` and
   `rate` exactly as they were, in Debug and in ReleaseFast.
8. `Tone.resolveCoeff` is total over the three filter cases:
   `.inherit_segment` yields the segment coefficient, `.bypass` yields exactly
   `0.0` — which the existing one-pole treats as a bit-for-bit bypass — and
   `.coefficient(c)` yields `c` when `c` is finite and in `[0, 1)` and
   `error.BadCoefficient` otherwise. `Tone.gain` must be finite and
   non-negative or note-on refuses with `error.BadGain`.
9. The resolved coefficient is prepared **once, outside the frame loop**. No
   transcendental, allocation, logging, locking, cold-state read or `@import`
   may enter the per-frame path.
10. Per-frame order is unchanged: reader x envelope x voice gain, then the
    one-pole filter, then declick. `Tone.gain` folds into the voice gain at
    note-on, not into a new multiply inside the loop.

## Definition of done

Write tests named after the invariant each defends, covering at least:
full-range byte compatibility; a one-frame range; forward `end` exclusion;
reverse `start` exclusion; refused-advance non-mutation; default `Tone` byte
compatibility; explicit `.bypass`; an explicit coefficient; non-finite and
out-of-domain coefficient and gain refusals; invalid range refusals; and exact
`Hot` size and field offsets.

Run, and record the exact exit result of each:

```
zig build test
zig build test-release
```

Do not bless a golden. If `products/player/golden.zig` disagrees with your
build, stop and report the first differing value and your diagnosis.

## Return contract

Return, in this order:

1. a single unified diff of your change, in one ```diff fenced block, applying
   cleanly to the frozen HEAD;
2. the files you changed;
3. the invariants you demonstrated, each with the test that demonstrates it;
4. the exact commands you ran and their exit results;
5. any criterion you did **not** measure, stated rather than implied green.

The benchmark score does not depend on prose style. It depends on the diff
applying, the build passing, the hidden invariant suite passing, and no
out-of-scope file changing.
