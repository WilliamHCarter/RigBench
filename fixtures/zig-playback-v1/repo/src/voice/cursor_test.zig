const std = @import("std");
const Cursor = @import("cursor.zig").Cursor;

test "direction follows the sign of rate" {
    try std.testing.expectEqual(.forward, Cursor.start(0, 1.0).mode());
    try std.testing.expectEqual(.reverse, Cursor.start(4, -1.0).mode());
}

test "forward advance stops before the upper bound" {
    var c = Cursor.start(0, 1.0);
    try c.advance(0, 3);
    try std.testing.expectEqual(@as(u64, 1), c.frame);
    try c.advance(0, 3);
    try std.testing.expectEqual(@as(u64, 2), c.frame);
    try std.testing.expectError(error.Exhausted, c.advance(0, 3));
}

test "reverse advance stops at the lower bound" {
    var c = Cursor.start(2, -1.0);
    try c.advance(1, 3);
    try std.testing.expectEqual(@as(u64, 1), c.frame);
    try std.testing.expectError(error.Exhausted, c.advance(1, 3));
}

test "a refused advance mutates nothing" {
    var c = Cursor.start(2, 1.0);
    c.phase = 0.25;
    const before = c;
    try std.testing.expectError(error.Exhausted, c.advance(0, 3));
    try std.testing.expectEqual(before.frame, c.frame);
    try std.testing.expectEqual(before.phase, c.phase);
    try std.testing.expectEqual(before.rate, c.rate);
}

test "a refused reverse advance below zero does not underflow" {
    var c = Cursor.start(0, -1.0);
    try std.testing.expectError(error.Exhausted, c.advance(0, 8));
    try std.testing.expectEqual(@as(u64, 0), c.frame);
}

test "a forward advance that would overflow u64 is refused" {
    var c = Cursor.start(std.math.maxInt(u64), 1.0);
    try std.testing.expectError(error.Exhausted, c.advance(0, std.math.maxInt(u64)));
    try std.testing.expectEqual(@as(u64, std.math.maxInt(u64)), c.frame);
}

test "a zero or non-finite rate is refused by name" {
    var c = Cursor.start(0, 0.0);
    try std.testing.expectError(error.BadRate, c.advance(0, 8));
    c.rate = std.math.nan(f64);
    try std.testing.expectError(error.BadRate, c.advance(0, 8));
    c.rate = std.math.inf(f64);
    try std.testing.expectError(error.BadRate, c.advance(0, 8));
}

test "fractional rates accumulate phase without losing a frame" {
    var c = Cursor.start(0, 0.5);
    try c.advance(0, 8);
    try std.testing.expectEqual(@as(u64, 0), c.frame);
    try std.testing.expectEqual(@as(f64, 0.5), c.phase);
    try c.advance(0, 8);
    try std.testing.expectEqual(@as(u64, 1), c.frame);
    try std.testing.expectEqual(@as(f64, 0.0), c.phase);
}

test "taps never reach the upper bound" {
    const c = Cursor.start(4, 1.0);
    const t = c.taps(0, 5);
    try std.testing.expectEqual(@as(u64, 4), t[0]);
    try std.testing.expectEqual(@as(u64, 4), t[1]);
}

test "taps never fall below the lower bound" {
    const c = Cursor.start(2, -1.0);
    const t = c.taps(2, 8);
    try std.testing.expectEqual(@as(u64, 2), t[0]);
    try std.testing.expectEqual(@as(u64, 3), t[1]);
}

test "isCoherent refuses phase outside [0,1), a frame outside bounds, and a bad rate" {
    var c = Cursor.start(1, 1.0);
    try std.testing.expect(c.isCoherent(0, 4));
    c.phase = 1.0;
    try std.testing.expect(!c.isCoherent(0, 4));
    c.phase = -0.0001;
    try std.testing.expect(!c.isCoherent(0, 4));
    c = Cursor.start(4, 1.0);
    try std.testing.expect(!c.isCoherent(0, 4));
    c = Cursor.start(0, 1.0);
    try std.testing.expect(!c.isCoherent(1, 4));
    c.rate = 0;
    try std.testing.expect(!c.isCoherent(0, 4));
}
