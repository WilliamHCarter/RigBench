const std = @import("std");
const tone_mod = @import("tone.zig");
const Tone = tone_mod.Tone;

test "the default Tone inherits the segment coefficient and unity gain" {
    const t = Tone{};
    try std.testing.expectEqual(@as(f32, 1.0), t.gain);
    try std.testing.expectEqual(tone_mod.Filter.inherit_segment, t.filter);
    try std.testing.expectEqual(@as(f32, 0.25), try t.resolveCoeff(0.25));
    // Bit-for-bit, not merely equal: the default path must not perturb the
    // inherited value.
    try std.testing.expectEqual(
        @as(u32, @bitCast(@as(f32, 0.25))),
        @as(u32, @bitCast(try t.resolveCoeff(0.25))),
    );
}

test "bypass resolves to exactly positive zero" {
    const t = Tone{ .filter = .bypass };
    const c = try t.resolveCoeff(0.9);
    try std.testing.expectEqual(@as(u32, 0), @as(u32, @bitCast(c)));
}

test "an explicit coefficient inside [0,1) is used verbatim" {
    const t = Tone{ .filter = .{ .coefficient = 0.5 } };
    try std.testing.expectEqual(@as(f32, 0.5), try t.resolveCoeff(0.25));
    const zero = Tone{ .filter = .{ .coefficient = 0.0 } };
    try std.testing.expectEqual(@as(u32, 0), @as(u32, @bitCast(try zero.resolveCoeff(0.25))));
}

test "an out-of-domain explicit coefficient is refused rather than clamped" {
    for ([_]f32{ 1.0, 1.5, -0.001, std.math.inf(f32), -std.math.inf(f32) }) |c| {
        const t = Tone{ .filter = .{ .coefficient = c } };
        try std.testing.expectError(error.BadCoefficient, t.resolveCoeff(0.25));
    }
    const nan_tone = Tone{ .filter = .{ .coefficient = std.math.nan(f32) } };
    try std.testing.expectError(error.BadCoefficient, nan_tone.resolveCoeff(0.25));
}

test "inheriting an out-of-domain segment coefficient is refused" {
    const t = Tone{};
    try std.testing.expectError(error.BadCoefficient, t.resolveCoeff(1.0));
    try std.testing.expectError(error.BadCoefficient, t.resolveCoeff(std.math.nan(f32)));
    try std.testing.expectError(error.BadCoefficient, t.resolveCoeff(-0.5));
}

test "bypass does not inspect the segment coefficient at all" {
    const t = Tone{ .filter = .bypass };
    try std.testing.expectEqual(@as(f32, 0.0), try t.resolveCoeff(std.math.nan(f32)));
}

test "a non-finite or negative gain is refused by name" {
    for ([_]f32{ std.math.nan(f32), std.math.inf(f32), -1.0 }) |g| {
        const t = Tone{ .gain = g };
        try std.testing.expectError(error.BadGain, t.resolveCoeff(0.25));
    }
    const zero_gain = Tone{ .gain = 0.0 };
    try std.testing.expectEqual(@as(f32, 0.25), try zero_gain.resolveCoeff(0.25));
}
