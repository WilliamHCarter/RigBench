const std = @import("std");
const player = @import("player.zig");
const golden = @import("golden.zig");

test "the reference render matches the frozen golden digest" {
    const hex = try player.digestHex();
    try std.testing.expectEqualStrings(golden.reference_digest_hex, &hex);
}

test "the first eight left-plane frames match the frozen bit patterns" {
    var out: player.Output = .{};
    try player.renderReference(&out);
    for (golden.reference_head_bits, 0..) |want, i| {
        try std.testing.expectEqual(want, @as(u32, @bitCast(out.left[i])));
    }
}

test "the reference render stops where the golden says it stops" {
    var out: player.Output = .{};
    try player.renderReference(&out);
    try std.testing.expectEqual(golden.reference_written, out.written);
}
