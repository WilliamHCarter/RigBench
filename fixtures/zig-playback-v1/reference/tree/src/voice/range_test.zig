const std = @import("std");
const FrameRange = @import("range.zig").FrameRange;

test "full spans the whole sample as a half-open interval" {
    const r = FrameRange.full(8);
    try std.testing.expectEqual(@as(u64, 0), r.start);
    try std.testing.expectEqual(@as(u64, 8), r.end);
    try std.testing.expectEqual(@as(u64, 8), r.len());
}

test "a range is valid when non-empty and inside the sample" {
    try std.testing.expect((FrameRange{ .start = 0, .end = 8 }).validate(8));
    try std.testing.expect((FrameRange{ .start = 3, .end = 4 }).validate(8));
    try std.testing.expect((FrameRange{ .start = 7, .end = 8 }).validate(8));
}

test "an empty or inverted range is invalid" {
    try std.testing.expect(!(FrameRange{ .start = 4, .end = 4 }).validate(8));
    try std.testing.expect(!(FrameRange{ .start = 5, .end = 4 }).validate(8));
    try std.testing.expect(!(FrameRange{ .start = 0, .end = 0 }).validate(8));
}

test "a range past the sample end is invalid rather than clamped" {
    try std.testing.expect(!(FrameRange{ .start = 0, .end = 9 }).validate(8));
    try std.testing.expect(!(FrameRange{ .start = 8, .end = 9 }).validate(8));
    // end == frames is the legal upper edge, because end is never read.
    try std.testing.expect((FrameRange{ .start = 0, .end = 8 }).validate(8));
}

test "a one-frame range is valid and has length one" {
    const r = FrameRange{ .start = 5, .end = 6 };
    try std.testing.expect(r.validate(8));
    try std.testing.expectEqual(@as(u64, 1), r.len());
}
