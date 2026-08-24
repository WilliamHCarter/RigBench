//! `core` module root. Every file under src/core/ is reached from here, and is
//! referenced twice: once as a re-export and once inside `test {}` so a module
//! that compiles but whose tests never execute is a visible failure rather than
//! a silent pass.
pub const sample = @import("sample.zig");

pub const SampleData = sample.SampleData;
pub const Segment = sample.Segment;
pub const pad_frames = sample.pad_frames;

test {
    _ = sample;
    _ = @import("sample_test.zig");
}
