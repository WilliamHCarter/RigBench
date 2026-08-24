const std = @import("std");
const core = @import("core");
const engine = @import("engine.zig");
const FrameRange = @import("range.zig").FrameRange;
const Tone = @import("tone.zig").Tone;

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

// --- the S2 seam ---------------------------------------------------------

fn renderRamp(cold: Cold, rate: f64, n: usize, buf: []f32) !usize {
    var r: Ramp = .{};
    const s = r.init();
    var v = Voice.idle();
    try v.noteOn(&s, cold, rate, 1, 7);
    var rr: [32]f32 = undefined;
    const out = [2][]f32{ buf[0..n], rr[0..n] };
    return v.renderBlock(&s, &out);
}

test "a null range still plays the whole sample" {
    var buf: [16]f32 = undefined;
    const n = try renderRamp(unity_cold, 1.0, 16, &buf);
    try std.testing.expectEqual(@as(usize, 8), n);
    try std.testing.expectEqual(@as(f32, 0), buf[0]);
    try std.testing.expectEqual(@as(f32, 7), buf[7]);
}

test "forward playback of a sub-range never reads the range end" {
    var cold = unity_cold;
    cold.range = .{ .start = 2, .end = 5 };
    var buf: [16]f32 = undefined;
    const n = try renderRamp(cold, 1.0, 16, &buf);
    try std.testing.expectEqual(@as(usize, 3), n);
    try std.testing.expectEqual(@as(f32, 2), buf[0]);
    try std.testing.expectEqual(@as(f32, 3), buf[1]);
    try std.testing.expectEqual(@as(f32, 4), buf[2]);
    for (buf[0..n]) |v| try std.testing.expect(v < 5.0);
}

test "reverse playback of a sub-range starts at end-1 and never reads below start" {
    var cold = unity_cold;
    cold.range = .{ .start = 2, .end = 5 };
    var buf: [16]f32 = undefined;
    const n = try renderRamp(cold, -1.0, 16, &buf);
    try std.testing.expectEqual(@as(usize, 3), n);
    try std.testing.expectEqual(@as(f32, 4), buf[0]);
    try std.testing.expectEqual(@as(f32, 2), buf[2]);
    for (buf[0..n]) |v| try std.testing.expect(v >= 2.0);
}

test "a one-frame range renders exactly one frame in either direction" {
    var cold = unity_cold;
    cold.range = .{ .start = 6, .end = 7 };
    var buf: [16]f32 = undefined;
    try std.testing.expectEqual(@as(usize, 1), try renderRamp(cold, 1.0, 16, &buf));
    try std.testing.expectEqual(@as(f32, 6), buf[0]);
    try std.testing.expectEqual(@as(usize, 1), try renderRamp(cold, -1.0, 16, &buf));
    try std.testing.expectEqual(@as(f32, 6), buf[0]);
}

test "an invalid range is refused by name and never clamped" {
    var r: Ramp = .{};
    const s = r.init();
    var v = Voice.idle();
    const bad = [_]FrameRange{
        .{ .start = 4, .end = 4 },
        .{ .start = 5, .end = 4 },
        .{ .start = 0, .end = 9 },
        .{ .start = 8, .end = 9 },
    };
    for (bad) |range| {
        var cold = unity_cold;
        cold.range = range;
        try std.testing.expectError(error.InvalidRange, v.noteOn(&s, cold, 1.0, 1, 7));
        try std.testing.expect(!v.isActive());
    }
}

test "the default Tone leaves the render bit-identical to unity" {
    var cold = unity_cold;
    cold.segment.filter_coeff = 0.5;
    var plain: [8]f32 = undefined;
    _ = try renderRamp(cold, 1.0, 8, &plain);

    cold.tone = .{};
    var toned: [8]f32 = undefined;
    _ = try renderRamp(cold, 1.0, 8, &toned);

    for (plain, toned) |a, b| {
        try std.testing.expectEqual(@as(u32, @bitCast(a)), @as(u32, @bitCast(b)));
    }
}

test "explicit bypass is bit-identical to a zero coefficient" {
    var filtered = unity_cold;
    filtered.segment.filter_coeff = 0.5;
    var bypassed = filtered;
    bypassed.tone = .{ .filter = .bypass };

    var a: [8]f32 = undefined;
    var b: [8]f32 = undefined;
    _ = try renderRamp(bypassed, 1.0, 8, &a);
    _ = try renderRamp(unity_cold, 1.0, 8, &b);
    for (a, b) |x, y| {
        try std.testing.expectEqual(@as(u32, @bitCast(x)), @as(u32, @bitCast(y)));
    }
}

test "an explicit coefficient overrides the segment coefficient" {
    var cold = unity_cold;
    cold.segment.filter_coeff = 0.0;
    cold.tone = .{ .filter = .{ .coefficient = 0.5 } };
    var buf: [4]f32 = undefined;
    _ = try renderRamp(cold, 1.0, 4, &buf);
    try std.testing.expectEqual(@as(f32, 0.0), buf[0]);
    try std.testing.expectEqual(@as(f32, 0.5), buf[1]);
    try std.testing.expectEqual(@as(f32, 1.25), buf[2]);
    try std.testing.expectEqual(@as(f32, 2.125), buf[3]);
}

test "Tone gain folds into the voice gain and unity gain is bit-identical" {
    var half = unity_cold;
    half.tone = .{ .gain = 0.5 };
    var buf: [4]f32 = undefined;
    _ = try renderRamp(half, 1.0, 4, &buf);
    try std.testing.expectEqual(@as(f32, 1.0), buf[2]);

    var unity = unity_cold;
    unity.tone = .{ .gain = 1.0 };
    var a: [8]f32 = undefined;
    var b: [8]f32 = undefined;
    _ = try renderRamp(unity, 1.0, 8, &a);
    _ = try renderRamp(unity_cold, 1.0, 8, &b);
    for (a, b) |x, y| {
        try std.testing.expectEqual(@as(u32, @bitCast(x)), @as(u32, @bitCast(y)));
    }
}

test "a bad Tone gain or coefficient is refused by name at note-on" {
    var r: Ramp = .{};
    const s = r.init();
    var v = Voice.idle();

    var cold = unity_cold;
    cold.tone = .{ .gain = std.math.nan(f32) };
    try std.testing.expectError(error.BadGain, v.noteOn(&s, cold, 1.0, 1, 7));

    cold = unity_cold;
    cold.tone = .{ .gain = -1.0 };
    try std.testing.expectError(error.BadGain, v.noteOn(&s, cold, 1.0, 1, 7));

    cold = unity_cold;
    cold.tone = .{ .filter = .{ .coefficient = 1.0 } };
    try std.testing.expectError(error.BadCoefficient, v.noteOn(&s, cold, 1.0, 1, 7));

    cold = unity_cold;
    cold.tone = .{ .filter = .{ .coefficient = std.math.nan(f32) } };
    try std.testing.expectError(error.BadCoefficient, v.noteOn(&s, cold, 1.0, 1, 7));

    try std.testing.expect(!v.isActive());
}

test "the seam did not grow Hot" {
    try std.testing.expectEqual(@as(usize, 72), @sizeOf(Hot));
    try std.testing.expectEqual(@as(usize, 68), @offsetOf(Hot, "handle"));
}
