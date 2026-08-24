//! Behavioural invariant controls.
//!
//! The sample used here has value == index at every audio frame, and every
//! multiplier on the path is unity, so a rendered frame *is* the index that was
//! read. Read-position claims are therefore observed in the output rather than
//! asserted about the implementation.

const std = @import("std");
const core = @import("core");
const voice = @import("voice");
const player = @import("player");

const Voice = voice.Voice;
const Cold = voice.Cold;
const Cursor = voice.Cursor;
const FrameRange = voice.FrameRange;
const Tone = voice.Tone;
const pad = core.pad_frames;

const frames: usize = 12;

const Ramp = struct {
    left: [pad + frames + pad]f32 = [_]f32{0} ** (pad + frames + pad),
    right: [pad + frames + pad]f32 = [_]f32{0} ** (pad + frames + pad),
    planes: [2][]const f32 = undefined,

    fn init(self: *Ramp) core.SampleData {
        for (0..frames) |i| {
            self.left[pad + i] = @floatFromInt(i);
            self.right[pad + i] = @floatFromInt(i);
        }
        self.planes = .{ &self.left, &self.right };
        return .{
            .channels = 2,
            .frames = frames,
            .sample_rate = 48_000,
            .pad = pad,
            .planes = &self.planes,
        };
    }
};

fn unity() Cold {
    return .{
        .segment = .{ .start = 0, .frames = frames, .filter_coeff = 0.0 },
        .base_gain = 1.0,
    };
}

const Trace = struct {
    buf: [64]f32 = [_]f32{0} ** 64,
    n: usize = 0,
    fn slice(self: *const Trace) []const f32 {
        return self.buf[0..self.n];
    }
};

fn render(cold: Cold, rate: f64, want: usize) !Trace {
    var r: Ramp = .{};
    const s = r.init();
    var v = Voice.idle();
    try v.noteOn(&s, cold, rate, 1, 3);
    var t: Trace = .{};
    var other: [64]f32 = undefined;
    const out = [2][]f32{ t.buf[0..want], other[0..want] };
    t.n = v.renderBlock(&s, &out);
    return t;
}

// --- range exclusion -----------------------------------------------------

test "forward playback of [2,5) reads 2,3,4 and never 5" {
    var cold = unity();
    cold.range = .{ .start = 2, .end = 5 };
    const t = try render(cold, 1.0, 32);
    try std.testing.expectEqual(@as(usize, 3), t.n);
    try std.testing.expectEqualSlices(f32, &[_]f32{ 2, 3, 4 }, t.slice());
}

test "reverse playback of [2,5) reads 4,3,2 and never 1" {
    var cold = unity();
    cold.range = .{ .start = 2, .end = 5 };
    const t = try render(cold, -1.0, 32);
    try std.testing.expectEqual(@as(usize, 3), t.n);
    try std.testing.expectEqualSlices(f32, &[_]f32{ 4, 3, 2 }, t.slice());
}

test "a range ending at the sample end reads the last frame and stops" {
    var cold = unity();
    cold.range = .{ .start = frames - 2, .end = frames };
    const t = try render(cold, 1.0, 32);
    try std.testing.expectEqualSlices(f32, &[_]f32{ 10, 11 }, t.slice());
}

test "a one-frame range renders exactly one frame, forward and reverse" {
    var cold = unity();
    cold.range = .{ .start = 7, .end = 8 };
    const fwd = try render(cold, 1.0, 32);
    try std.testing.expectEqualSlices(f32, &[_]f32{7}, fwd.slice());
    const rev = try render(cold, -1.0, 32);
    try std.testing.expectEqualSlices(f32, &[_]f32{7}, rev.slice());
}

test "a fractional forward rate never interpolates past the range end" {
    var cold = unity();
    cold.range = .{ .start = 2, .end = 5 };
    const t = try render(cold, 0.5, 32);
    try std.testing.expect(t.n > 0);
    for (t.slice()) |v| {
        try std.testing.expect(v >= 2.0);
        try std.testing.expect(v < 5.0);
    }
}

test "a fractional reverse rate never interpolates below the range start" {
    var cold = unity();
    cold.range = .{ .start = 4, .end = 9 };
    const t = try render(cold, -0.5, 32);
    try std.testing.expect(t.n > 0);
    for (t.slice()) |v| {
        try std.testing.expect(v >= 4.0);
        try std.testing.expect(v < 9.0);
    }
}

test "a rate that would step over the range end retires rather than clamping" {
    var cold = unity();
    cold.range = .{ .start = 2, .end = 5 };
    const t = try render(cold, 2.0, 32);
    try std.testing.expectEqualSlices(f32, &[_]f32{ 2, 4 }, t.slice());
}

// --- refusals ------------------------------------------------------------

test "every invalid range is refused by name and leaves the voice inactive" {
    var r: Ramp = .{};
    const s = r.init();
    const bad = [_]FrameRange{
        .{ .start = 0, .end = 0 },
        .{ .start = 3, .end = 3 },
        .{ .start = 6, .end = 5 },
        .{ .start = 0, .end = frames + 1 },
        .{ .start = frames, .end = frames + 1 },
        .{ .start = frames, .end = frames },
        .{ .start = 0, .end = std.math.maxInt(u64) },
        .{ .start = std.math.maxInt(u64), .end = std.math.maxInt(u64) },
    };
    for (bad) |range| {
        var v = Voice.idle();
        var cold = unity();
        cold.range = range;
        try std.testing.expectError(error.InvalidRange, v.noteOn(&s, cold, 1.0, 1, 3));
        try std.testing.expect(!v.isActive());
    }
}

test "a valid range at the exact upper edge is accepted" {
    var cold = unity();
    cold.range = .{ .start = 0, .end = frames };
    const t = try render(cold, 1.0, 32);
    try std.testing.expectEqual(@as(usize, frames), t.n);
}

test "a refused advance mutates no cursor field, at every field" {
    const r = FrameRange{ .start = 2, .end = 5 };
    var c = Cursor.start(4, 1.0);
    c.phase = 0.375;
    const before = c;
    try std.testing.expectError(error.Exhausted, c.advance(r));
    try std.testing.expectEqual(before.frame, c.frame);
    try std.testing.expectEqual(
        @as(u64, @bitCast(before.phase)),
        @as(u64, @bitCast(c.phase)),
    );
    try std.testing.expectEqual(
        @as(u64, @bitCast(before.rate)),
        @as(u64, @bitCast(c.rate)),
    );
}

test "a refused reverse advance at the range start mutates nothing" {
    const r = FrameRange{ .start = 2, .end = 5 };
    var c = Cursor.start(2, -1.0);
    c.phase = 0.75;
    const before = c;
    try std.testing.expectError(error.Exhausted, c.advance(r));
    try std.testing.expectEqual(before.frame, c.frame);
    try std.testing.expectEqual(
        @as(u64, @bitCast(before.phase)),
        @as(u64, @bitCast(c.phase)),
    );
}

test "a refused advance mutates nothing at the u64 edges" {
    var lo = Cursor.start(0, -1.0);
    try std.testing.expectError(error.Exhausted, lo.advance(.{ .start = 0, .end = 8 }));
    try std.testing.expectEqual(@as(u64, 0), lo.frame);

    var hi = Cursor.start(std.math.maxInt(u64), 1.0);
    try std.testing.expectError(
        error.Exhausted,
        hi.advance(.{ .start = 0, .end = std.math.maxInt(u64) }),
    );
    try std.testing.expectEqual(@as(u64, std.math.maxInt(u64)), hi.frame);
}

test "a non-finite or zero rate is refused at note-on and at advance" {
    var r: Ramp = .{};
    const s = r.init();
    for ([_]f64{ 0.0, std.math.nan(f64), std.math.inf(f64), -std.math.inf(f64) }) |rate| {
        var v = Voice.idle();
        try std.testing.expectError(error.BadRate, v.noteOn(&s, unity(), rate, 1, 3));
        var c = Cursor.start(1, rate);
        try std.testing.expectError(error.BadRate, c.advance(.{ .start = 0, .end = 8 }));
    }
}

test "a non-finite or out-of-domain coefficient is refused, never canonicalized" {
    var r: Ramp = .{};
    const s = r.init();
    const bad = [_]f32{
        1.0,
        1.0000001,
        2.0,
        -0.0000001,
        -1.0,
        std.math.nan(f32),
        -std.math.nan(f32),
        std.math.inf(f32),
        -std.math.inf(f32),
    };
    for (bad) |c| {
        var v = Voice.idle();
        var cold = unity();
        cold.tone = .{ .filter = .{ .coefficient = c } };
        try std.testing.expectError(error.BadCoefficient, v.noteOn(&s, cold, 1.0, 1, 3));
        try std.testing.expect(!v.isActive());

        var w = Voice.idle();
        var inherited = unity();
        inherited.segment.filter_coeff = c;
        try std.testing.expectError(error.BadCoefficient, w.noteOn(&s, inherited, 1.0, 1, 3));
    }
}

test "a non-finite or negative tone gain is refused" {
    var r: Ramp = .{};
    const s = r.init();
    for ([_]f32{ std.math.nan(f32), std.math.inf(f32), -std.math.inf(f32), -0.5 }) |g| {
        var v = Voice.idle();
        var cold = unity();
        cold.tone = .{ .gain = g };
        try std.testing.expectError(error.BadGain, v.noteOn(&s, cold, 1.0, 1, 3));
        try std.testing.expect(!v.isActive());
    }
}

test "a zero tone gain is legal and silences the voice exactly" {
    var cold = unity();
    cold.tone = .{ .gain = 0.0 };
    const t = try render(cold, 1.0, 8);
    try std.testing.expect(t.n > 0);
    for (t.slice()) |v| try std.testing.expectEqual(@as(u32, 0), @as(u32, @bitCast(v)));
}

// --- tone composition ----------------------------------------------------

test "the default tone is bit-identical to no tone at every coefficient" {
    for ([_]f32{ 0.0, 0.125, 0.5, 0.9375 }) |c| {
        var base = unity();
        base.segment.filter_coeff = c;
        const plain = try render(base, 0.75, 16);

        var toned = base;
        toned.tone = Tone{};
        const with = try render(toned, 0.75, 16);

        try std.testing.expectEqual(plain.n, with.n);
        for (plain.slice(), with.slice()) |a, b| {
            try std.testing.expectEqual(@as(u32, @bitCast(a)), @as(u32, @bitCast(b)));
        }
    }
}

test "bypass is bit-identical to a zero segment coefficient" {
    var bypassed = unity();
    bypassed.segment.filter_coeff = 0.75;
    bypassed.tone = .{ .filter = .bypass };
    const a = try render(bypassed, 0.75, 16);

    var zero = unity();
    zero.segment.filter_coeff = 0.0;
    const b = try render(zero, 0.75, 16);

    try std.testing.expectEqual(a.n, b.n);
    for (a.slice(), b.slice()) |x, y| {
        try std.testing.expectEqual(@as(u32, @bitCast(x)), @as(u32, @bitCast(y)));
    }
}

test "an explicit coefficient overrides the segment and is not merely added to it" {
    var overridden = unity();
    overridden.segment.filter_coeff = 0.5;
    overridden.tone = .{ .filter = .{ .coefficient = 0.25 } };
    const a = try render(overridden, 1.0, 8);

    var direct = unity();
    direct.segment.filter_coeff = 0.25;
    const b = try render(direct, 1.0, 8);

    for (a.slice(), b.slice()) |x, y| {
        try std.testing.expectEqual(@as(u32, @bitCast(x)), @as(u32, @bitCast(y)));
    }

    // And it is genuinely a different signal from the segment's own value.
    var seg = unity();
    seg.segment.filter_coeff = 0.5;
    const c = try render(seg, 1.0, 8);
    var differs = false;
    for (a.slice(), c.slice()) |x, y| {
        if (@as(u32, @bitCast(x)) != @as(u32, @bitCast(y))) differs = true;
    }
    try std.testing.expect(differs);
}

test "tone gain multiplies the voice gain rather than replacing it" {
    var cold = unity();
    cold.base_gain = 0.5;
    cold.tone = .{ .gain = 0.5 };
    const t = try render(cold, 1.0, 8);
    // frame 4 of a value==index ramp at 0.25 total gain
    try std.testing.expectEqual(@as(f32, 1.0), t.buf[4]);
}

test "the one-pole runs after the envelope, not before it" {
    // With a *constant* gain this order is unobservable: a one-pole is linear
    // and a scalar commutes with it, so filter-then-gain and gain-then-filter
    // produce identical bytes. A time-varying gain does not commute, so the
    // envelope is ramped here. That is the only reason this test can fail.
    var cold = unity();
    cold.segment.filter_coeff = 0.5;
    cold.range = .{ .start = 4, .end = 8 };

    var r: Ramp = .{};
    const s = r.init();
    var v = Voice.idle();
    try v.noteOn(&s, cold, 1.0, 1, 3);
    v.hot.env_level = 0.5;
    v.hot.env_rate = 0.25;

    var l: [4]f32 = undefined;
    var other: [4]f32 = undefined;
    const out = [2][]f32{ &l, &other };
    try std.testing.expectEqual(@as(usize, 4), v.renderBlock(&s, &out));

    // x = 4,5,6,7 ; env = 0.5,0.75,1,1 ; c = 0.5
    // reader x envelope, then one-pole:  1.0, 2.375, 4.1875, 5.59375
    // one-pole, then envelope:           1.0, 2.625, 4.75,   5.875
    try std.testing.expectEqualSlices(f32, &[_]f32{ 1.0, 2.375, 4.1875, 5.59375 }, &l);
}

test "tone gain is folded in at note-on, not applied again inside the loop" {
    // A second per-frame multiply by tone.gain would square it. 0.5 squared is
    // 0.25, which this catches; 1.0 squared is 1.0, which is why the witness
    // uses a non-unity gain.
    var cold = unity();
    cold.base_gain = 1.0;
    cold.tone = .{ .gain = 0.5 };
    cold.range = .{ .start = 4, .end = 8 };
    const t = try render(cold, 1.0, 4);
    try std.testing.expectEqualSlices(f32, &[_]f32{ 2.0, 2.5, 3.0, 3.5 }, t.slice());
}

test "the filter is stateful across frames and per channel" {
    var cold = unity();
    cold.segment.filter_coeff = 0.5;
    var r: Ramp = .{};
    const s = r.init();
    var v = Voice.idle();
    try v.noteOn(&s, cold, 1.0, 1, 3);
    var l: [4]f32 = undefined;
    var rr: [4]f32 = undefined;
    const out = [2][]f32{ &l, &rr };
    _ = v.renderBlock(&s, &out);
    try std.testing.expectEqualSlices(f32, &[_]f32{ 0, 0.5, 1.25, 2.125 }, &l);
    try std.testing.expectEqualSlices(f32, &l, &rr);
}

// --- default-path byte compatibility ------------------------------------

test "the Player golden is unchanged by the seam" {
    const hex = try player.digestHex();
    try std.testing.expectEqualStrings(player.golden.reference_digest_hex, &hex);
}

test "the Player render still stops where it stopped before the seam" {
    var out: player.Output = .{};
    try player.renderReference(&out);
    try std.testing.expectEqual(player.golden.reference_written, out.written);
    for (player.golden.reference_head_bits, 0..) |want, i| {
        try std.testing.expectEqual(want, @as(u32, @bitCast(out.left[i])));
    }
}

test "an explicit full range is bit-identical to a null range" {
    const null_range = try render(unity(), 0.75, 32);
    var explicit = unity();
    explicit.range = FrameRange.full(frames);
    const full = try render(explicit, 0.75, 32);
    try std.testing.expectEqual(null_range.n, full.n);
    for (null_range.slice(), full.slice()) |a, b| {
        try std.testing.expectEqual(@as(u32, @bitCast(a)), @as(u32, @bitCast(b)));
    }
}

test "FrameRange.full is the whole sample and validates against it" {
    const r = FrameRange.full(frames);
    try std.testing.expectEqual(@as(u64, 0), r.start);
    try std.testing.expectEqual(@as(u64, frames), r.end);
    try std.testing.expectEqual(@as(u64, frames), r.len());
    try std.testing.expect(r.validate(frames));
    try std.testing.expect(!r.validate(frames - 1));
}
