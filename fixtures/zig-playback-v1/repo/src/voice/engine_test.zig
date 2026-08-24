const std = @import("std");
const core = @import("core");
const engine = @import("engine.zig");

const Voice = engine.Voice;
const Hot = engine.Hot;
const Cold = engine.Cold;
const pad = core.pad_frames;

pub const Ramp = struct {
    left: [pad + 8 + pad]f32 = [_]f32{0} ** (pad + 8 + pad),
    right: [pad + 8 + pad]f32 = [_]f32{0} ** (pad + 8 + pad),
    planes: [2][]const f32 = undefined,

    /// A sample whose value at audio index i is exactly i. With unity gain,
    /// unity envelope, a zero coefficient and unity declick, each rendered
    /// output frame is the index that was read -- so the output *is* the read
    /// trace, with no instrumentation in the audio path.
    pub fn init(self: *Ramp) core.SampleData {
        for (0..8) |i| {
            self.left[pad + i] = @floatFromInt(i);
            self.right[pad + i] = @floatFromInt(i);
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

pub const unity_cold = Cold{
    .segment = .{ .start = 0, .frames = 8, .filter_coeff = 0.0 },
    .base_gain = 1.0,
};

test "Hot is exactly 72 bytes" {
    try std.testing.expectEqual(@as(usize, 72), @sizeOf(Hot));
    try std.testing.expectEqual(@as(usize, 72), engine.hot_size);
}

test "Hot field offsets are unchanged" {
    try std.testing.expectEqual(@as(usize, 0), @offsetOf(Hot, "cursor"));
    try std.testing.expectEqual(@as(usize, 24), @offsetOf(Hot, "start_seq"));
    try std.testing.expectEqual(@as(usize, 32), @offsetOf(Hot, "env_level"));
    try std.testing.expectEqual(@as(usize, 36), @offsetOf(Hot, "env_rate"));
    try std.testing.expectEqual(@as(usize, 40), @offsetOf(Hot, "declick_gain"));
    try std.testing.expectEqual(@as(usize, 44), @offsetOf(Hot, "declick_step"));
    try std.testing.expectEqual(@as(usize, 48), @offsetOf(Hot, "voice_gain"));
    try std.testing.expectEqual(@as(usize, 52), @offsetOf(Hot, "filter_coeff"));
    try std.testing.expectEqual(@as(usize, 56), @offsetOf(Hot, "filter_z1"));
    try std.testing.expectEqual(@as(usize, 64), @offsetOf(Hot, "flags"));
    try std.testing.expectEqual(@as(usize, 68), @offsetOf(Hot, "handle"));
}

test "a voice retains no pointer" {
    inline for (@typeInfo(Cold).@"struct".fields) |f| {
        if (@typeInfo(f.type) == .pointer) {
            @compileError("Cold." ++ f.name ++ " is a pointer; a voice must retain none");
        }
    }
}

test "forward playback over the whole sample never reads the frame count" {
    var r: Ramp = .{};
    const s = r.init();
    var v = Voice.idle();
    try v.noteOn(&s, unity_cold, 1.0, 1, 7);

    var l: [16]f32 = undefined;
    var rr: [16]f32 = undefined;
    const out = [2][]f32{ &l, &rr };
    const n = v.renderBlock(&s, &out);

    try std.testing.expectEqual(@as(usize, 8), n);
    for (0..n) |i| try std.testing.expectEqual(@as(f32, @floatFromInt(i)), l[i]);
    try std.testing.expect(!v.isActive());
}

test "reverse playback starts at the last frame and never goes below zero" {
    var r: Ramp = .{};
    const s = r.init();
    var v = Voice.idle();
    try v.noteOn(&s, unity_cold, -1.0, 1, 7);

    var l: [16]f32 = undefined;
    var rr: [16]f32 = undefined;
    const out = [2][]f32{ &l, &rr };
    const n = v.renderBlock(&s, &out);

    try std.testing.expectEqual(@as(usize, 8), n);
    try std.testing.expectEqual(@as(f32, 7), l[0]);
    try std.testing.expectEqual(@as(f32, 0), l[7]);
    try std.testing.expect(!v.isActive());
}

test "a zero coefficient is a bit-for-bit bypass" {
    var r: Ramp = .{};
    const s = r.init();
    var v = Voice.idle();
    try v.noteOn(&s, unity_cold, 1.0, 1, 7);

    var l: [8]f32 = undefined;
    var rr: [8]f32 = undefined;
    const out = [2][]f32{ &l, &rr };
    _ = v.renderBlock(&s, &out);
    for (0..8) |i| {
        try std.testing.expectEqual(
            @as(u32, @bitCast(@as(f32, @floatFromInt(i)))),
            @as(u32, @bitCast(l[i])),
        );
    }
}

test "a nonzero coefficient filters and is stateful across frames" {
    var r: Ramp = .{};
    const s = r.init();
    var cold = unity_cold;
    cold.segment.filter_coeff = 0.5;
    var v = Voice.idle();
    try v.noteOn(&s, cold, 1.0, 1, 7);

    var l: [4]f32 = undefined;
    var rr: [4]f32 = undefined;
    const out = [2][]f32{ &l, &rr };
    _ = v.renderBlock(&s, &out);
    // y[n] = x[n] + 0.5*(y[n-1] - x[n]); x = 0,1,2,3 ; y0 = 0
    try std.testing.expectEqual(@as(f32, 0.0), l[0]);
    try std.testing.expectEqual(@as(f32, 0.5), l[1]);
    try std.testing.expectEqual(@as(f32, 1.25), l[2]);
    try std.testing.expectEqual(@as(f32, 2.125), l[3]);
}

test "note-on refuses an incoherent sample, a bad gain, a bad coefficient and a bad rate" {
    var r: Ramp = .{};
    var s = r.init();
    var v = Voice.idle();

    s.channels = 0;
    try std.testing.expectError(error.IncoherentSample, v.noteOn(&s, unity_cold, 1.0, 1, 7));
    s = r.init();

    var bad = unity_cold;
    bad.base_gain = std.math.nan(f32);
    try std.testing.expectError(error.BadGain, v.noteOn(&s, bad, 1.0, 1, 7));

    bad = unity_cold;
    bad.segment.filter_coeff = 1.0;
    try std.testing.expectError(error.BadCoefficient, v.noteOn(&s, bad, 1.0, 1, 7));
    bad.segment.filter_coeff = std.math.inf(f32);
    try std.testing.expectError(error.BadCoefficient, v.noteOn(&s, bad, 1.0, 1, 7));
    bad.segment.filter_coeff = -0.25;
    try std.testing.expectError(error.BadCoefficient, v.noteOn(&s, bad, 1.0, 1, 7));

    try std.testing.expectError(error.BadRate, v.noteOn(&s, unity_cold, 0.0, 1, 7));
    try std.testing.expect(!v.isActive());
}

test "gain scales the output and unity gain is bit-for-bit" {
    var r: Ramp = .{};
    const s = r.init();
    var cold = unity_cold;
    cold.base_gain = 0.5;
    var v = Voice.idle();
    try v.noteOn(&s, cold, 1.0, 1, 7);

    var l: [4]f32 = undefined;
    var rr: [4]f32 = undefined;
    const out = [2][]f32{ &l, &rr };
    _ = v.renderBlock(&s, &out);
    try std.testing.expectEqual(@as(f32, 1.0), l[2]);
}
