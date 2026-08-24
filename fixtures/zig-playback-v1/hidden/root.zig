//! AgentBench-01 hidden invariant suite for zig-playback-v1.
//!
//! Written independently of the visible suite. A candidate patch that passes
//! `zig build test` and fails here has produced tests that agree with itself.
//!
//! Every file here is reached twice, as a re-export and inside `test {}`.
pub const layout = @import("layout_test.zig");
pub const invariants = @import("invariants_test.zig");
pub const structural = @import("structural_test.zig");

test {
    _ = layout;
    _ = invariants;
    _ = structural;
}
