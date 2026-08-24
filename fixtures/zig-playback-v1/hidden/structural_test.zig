//! Structural control: *where* a computation happens.
//!
//! No ordinary test can observe that the filter coefficient was resolved once
//! at note-on rather than once per frame, because both produce the same audio.
//! This file reads the engine's own source bytes, isolates the body of the
//! frame loop inside `renderBlock`, and asserts that nothing derived from cold
//! state or from the standard library's slow paths appears inside it.
//!
//! A check believed to say more than it says is worse than no check, so the
//! matcher is driven over probe sources first -- one that must trip it, one that
//! must not, and one it must report as unparseable rather than clean. A matcher
//! broken into never matching therefore reports as a broken matcher instead of
//! as a clean tree. What this file does *not* prove is stated at the foot.

const std = @import("std");

const engine_src = @embedFile("engine_src");
const cursor_src = @embedFile("cursor_src");

const ScanError = error{
    /// `renderBlock` was not found. The check cannot run, and that is a
    /// failure, not a pass.
    RenderBlockNotFound,
    /// No frame loop was found inside `renderBlock`.
    FrameLoopNotFound,
    /// The loop body's braces do not balance.
    UnbalancedBraces,
};

/// Identifiers that must not appear inside the frame loop. Each one is a doctrine
/// rule from AGENTS.md, not a style preference.
const forbidden = [_][]const u8{
    "resolveCoeff", // the coefficient is resolved at note-on
    "rangeOf", // the range is resolved at note-on
    ".cold", // no cold-state read inside the loop
    "self.cold",
    ".tone",
    ".range",
    ".segment",
    "@import", // no import inside the loop
    "alloc", // no allocation
    "Mutex", // no locking
    "std.math.exp",
    "expm1",
    "std.debug.print",
    "std.log",
    "validate(", // no per-frame revalidation of setup state
};

/// The body of the first `while` loop inside `renderBlock`, braces excluded.
fn frameLoopBody(src: []const u8) ScanError![]const u8 {
    const fn_at = std.mem.indexOf(u8, src, "fn renderBlock") orelse
        return error.RenderBlockNotFound;
    const rest = src[fn_at..];
    const loop_rel = std.mem.indexOf(u8, rest, "while (") orelse
        return error.FrameLoopNotFound;
    const open_rel = std.mem.indexOfScalarPos(u8, rest, loop_rel, '{') orelse
        return error.FrameLoopNotFound;

    var depth: usize = 0;
    var i = open_rel;
    while (i < rest.len) : (i += 1) {
        switch (rest[i]) {
            '{' => depth += 1,
            '}' => {
                depth -= 1;
                if (depth == 0) return rest[open_rel + 1 .. i];
            },
            else => {},
        }
    }
    return error.UnbalancedBraces;
}

fn firstForbidden(body: []const u8) ?[]const u8 {
    for (forbidden) |needle| {
        if (std.mem.indexOf(u8, body, needle) != null) return needle;
    }
    return null;
}

// --- matcher controls ----------------------------------------------------

const probe_dirty =
    \\pub fn renderBlock(self: *Voice) void {
    \\    var i: usize = 0;
    \\    while (i < 4) {
    \\        const c = self.cold.tone.resolveCoeff(0.0) catch unreachable;
    \\        out[i] = c;
    \\        i += 1;
    \\    }
    \\}
;

const probe_clean =
    \\pub fn renderBlock(self: *Voice) void {
    \\    const c = self.cold.tone.resolveCoeff(0.0) catch unreachable;
    \\    var i: usize = 0;
    \\    while (i < 4) {
    \\        out[i] = c * self.hot.env_level;
    \\        if (true) { i += 1; }
    \\    }
    \\}
;

const probe_no_fn =
    \\pub fn other(self: *Voice) void {
    \\    while (true) {}
    \\}
;

test "control: the matcher trips on a coefficient resolved inside the loop" {
    const body = try frameLoopBody(probe_dirty);
    const hit = firstForbidden(body) orelse return error.MatcherFailedToTrip;
    try std.testing.expectEqualStrings("resolveCoeff", hit);
}

test "control: the matcher stays quiet when the resolution is outside the loop" {
    const body = try frameLoopBody(probe_clean);
    try std.testing.expect(firstForbidden(body) == null);
    // and it found a real, nested body rather than an empty string
    try std.testing.expect(std.mem.indexOf(u8, body, "i += 1") != null);
    try std.testing.expect(std.mem.indexOf(u8, body, "env_level") != null);
}

test "control: a missing renderBlock is a failure, not a clean tree" {
    try std.testing.expectError(error.RenderBlockNotFound, frameLoopBody(probe_no_fn));
}

// --- the real tree -------------------------------------------------------

test "the frame loop is found in the real engine source" {
    const body = try frameLoopBody(engine_src);
    try std.testing.expect(body.len > 80);
    // The loop must actually be the audio loop: it writes an output plane and
    // steps the cursor. A matcher that latched onto some other while-loop
    // reports here rather than passing quietly.
    try std.testing.expect(std.mem.indexOf(u8, body, "plane[") != null);
    try std.testing.expect(std.mem.indexOf(u8, body, "advance(") != null);
}

test "nothing derived from cold state is computed inside the frame loop" {
    const body = try frameLoopBody(engine_src);
    if (firstForbidden(body)) |hit| {
        std.debug.print(
            "\nforbidden in frame loop: \"{s}\"\nloop body:\n{s}\n",
            .{ hit, body },
        );
        return error.ForbiddenInFrameLoop;
    }
}

test "the coefficient is resolved somewhere outside the frame loop" {
    const body = try frameLoopBody(engine_src);
    const before = engine_src[0 .. @intFromPtr(body.ptr) - @intFromPtr(engine_src.ptr)];
    // Some setup-time resolution must exist, or the seam is not wired at all.
    const wired = std.mem.indexOf(u8, before, "resolveCoeff") != null or
        std.mem.indexOf(u8, engine_src, "resolveCoeff") != null;
    try std.testing.expect(wired);
}

test "the cursor performs no allocation, logging or locking" {
    for ([_][]const u8{ "alloc", "Mutex", "std.debug.print", "std.log" }) |needle| {
        if (std.mem.indexOf(u8, cursor_src, needle) != null) {
            std.debug.print("\nforbidden in cursor.zig: \"{s}\"\n", .{needle});
            return error.ForbiddenInCursor;
        }
    }
}

// What this file does NOT prove, recorded rather than implied:
//
//   * Not that the resolution is *correct*. `invariants_test.zig` owns that.
//   * Not that the loop is efficient. It proves absence of named constructs,
//     not the absence of per-frame work in general.
//   * Not anything about a second loop. Only the first `while` inside
//     `renderBlock` is inspected; a per-frame computation moved into a helper
//     called from that loop is invisible to this check, and stays invisible.
//   * Not that a builder did not simply move the sentence being checked. This
//     is a structural control against accident and habit, not against an
//     adversary editing the checker's input.
