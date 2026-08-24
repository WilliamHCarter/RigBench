const std = @import("std");
const sample = @import("sample.zig");
const SampleData = sample.SampleData;
const pad = sample.pad_frames;

/// Backing store for a 2-channel, 8-frame padded sample. Static so the fixture
/// needs no allocator and no test-only failure modes.
const Fixture = struct {
    left: [pad + 8 + pad]f32,
    right: [pad + 8 + pad]f32,
    planes: [2][]const f32,

    fn init(self: *Fixture) SampleData {
        self.left = [_]f32{0} ** (pad + 8 + pad);
        self.right = [_]f32{0} ** (pad + 8 + pad);
        for (0..8) |i| {
            self.left[pad + i] = @floatFromInt(i);
            self.right[pad + i] = @floatFromInt(100 + i);
        }
        self.planes = .{ &self.left, &self.right };
        return .{
            .channels = 2,
            .frames = 8,
            .sample_rate = 48_000,
            .pad = pad,
            .planes = &self.planes,
        };
    }
};

test "tap is audio-relative and the guard frame reads exact zero" {
    var f: Fixture = undefined;
    const s = f.init();
    try std.testing.expectEqual(@as(f32, 0), s.tap(0, 0));
    try std.testing.expectEqual(@as(f32, 7), s.tap(0, 7));
    try std.testing.expectEqual(@as(f32, 107), s.tap(1, 7));
    // The first guard frame, reached by an interpolation tap and never by a
    // legal cursor position.
    try std.testing.expectEqual(@as(u32, 0), @as(u32, @bitCast(s.tap(0, 8))));
}

test "isCoherent accepts the well-formed shape" {
    var f: Fixture = undefined;
    const s = f.init();
    try std.testing.expect(s.isCoherent());
}

test "isCoherent refuses zero channels" {
    var f: Fixture = undefined;
    var s = f.init();
    s.channels = 0;
    try std.testing.expect(!s.isCoherent());
}

test "isCoherent refuses a plane count that disagrees with channels" {
    var f: Fixture = undefined;
    var s = f.init();
    s.channels = 1;
    try std.testing.expect(!s.isCoherent());
}

test "isCoherent refuses a wrong-length plane on any channel" {
    var f: Fixture = undefined;
    var s = f.init();
    // Shorten only the second plane: a checker that inspects planes[0] and
    // stops answers true here.
    var short: [pad + 8 + pad - 1]f32 = [_]f32{0} ** (pad + 8 + pad - 1);
    var planes = [2][]const f32{ &f.left, &short };
    s.planes = &planes;
    try std.testing.expect(!s.isCoherent());
}

test "isCoherent refuses a pad that disagrees with the plane" {
    var f: Fixture = undefined;
    var s = f.init();
    s.pad = pad - 1;
    try std.testing.expect(!s.isCoherent());
}

test "isCoherent refuses nonzero guard frames at either end" {
    var f: Fixture = undefined;
    var s = f.init();
    f.left[0] = 1.0;
    try std.testing.expect(!s.isCoherent());
    f.left[0] = 0.0;
    try std.testing.expect(s.isCoherent());
    f.right[f.right.len - 1] = -0.0; // sign bit set: not exact zero by bit pattern
    try std.testing.expect(!s.isCoherent());
}

test "isCoherent refuses zero frames and zero sample rate" {
    var f: Fixture = undefined;
    var s = f.init();
    s.frames = 0;
    try std.testing.expect(!s.isCoherent());
    s = f.init();
    s.sample_rate = 0;
    try std.testing.expect(!s.isCoherent());
}
