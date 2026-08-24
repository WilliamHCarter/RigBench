//! The Player product: the one consumer whose output bytes are frozen.
//!
//! Player builds a fixed sample, starts one voice from default cold state, and
//! renders a fixed number of frames. Its output is a golden: any change to the
//! default path shows up here as a different digest. Player is deliberately
//! *not* owned by a bounded voice-engine task, so a builder cannot make its own
//! change look compatible by editing the consumer.

const std = @import("std");
const core = @import("core");
const voice = @import("voice");

pub const golden = @import("golden.zig");

pub const frames: usize = 64;
pub const render_frames: usize = 96;
pub const channels: usize = 2;

const pad = core.pad_frames;

pub const Reference = struct {
    left: [pad + frames + pad]f32 = [_]f32{0} ** (pad + frames + pad),
    right: [pad + frames + pad]f32 = [_]f32{0} ** (pad + frames + pad),
    planes: [channels][]const f32 = undefined,

    /// Deterministic integer-derived content: no libm, no float parsing, and
    /// the same bytes on every target.
    pub fn init(self: *Reference) core.SampleData {
        for (0..frames) |i| {
            const k: u32 = @intCast(i);
            const a: i32 = @intCast((k *% 2_654_435_761) % 2_048);
            const b: i32 = @intCast((k *% 40_503 +% 7) % 2_048);
            self.left[pad + i] = @as(f32, @floatFromInt(a - 1_024)) / 1_024.0;
            self.right[pad + i] = @as(f32, @floatFromInt(b - 1_024)) / 1_024.0;
        }
        self.planes = .{ &self.left, &self.right };
        return .{
            .channels = channels,
            .frames = frames,
            .sample_rate = 48_000,
            .pad = pad,
            .planes = &self.planes,
        };
    }
};

/// The default cold state the golden is defined over. A seam that adds fields
/// to `Cold` must leave this initializer meaning exactly what it means now.
pub fn defaultCold() voice.Cold {
    return .{
        .segment = .{ .start = 0, .frames = frames, .filter_coeff = 0.25 },
        .base_gain = 0.75,
    };
}

pub const Output = struct {
    left: [render_frames]f32 = [_]f32{0} ** render_frames,
    right: [render_frames]f32 = [_]f32{0} ** render_frames,
    written: usize = 0,
};

pub const RenderError = voice.engine.NoteOnError;

pub fn renderReference(out: *Output) RenderError!void {
    var ref: Reference = .{};
    const s = ref.init();
    var v = voice.Voice.idle();
    try v.noteOn(&s, defaultCold(), 0.75, 1, 1);
    const planes = [channels][]f32{ &out.left, &out.right };
    out.written = v.renderBlock(&s, &planes);
}

/// SHA-256 over the little-endian f32 bytes of both planes followed by the
/// written frame count. Digesting the count as well means a render that stops
/// early cannot collide with one that does not.
pub fn referenceDigest() RenderError![32]u8 {
    var out: Output = .{};
    try renderReference(&out);

    var h = std.crypto.hash.sha2.Sha256.init(.{});
    for ([_][]const f32{ &out.left, &out.right }) |plane| {
        for (plane) |v| {
            var bytes: [4]u8 = undefined;
            std.mem.writeInt(u32, &bytes, @bitCast(v), .little);
            h.update(&bytes);
        }
    }
    var count: [8]u8 = undefined;
    std.mem.writeInt(u64, &count, out.written, .little);
    h.update(&count);
    return h.finalResult();
}

pub fn digestHex() RenderError![64]u8 {
    const d = try referenceDigest();
    var hex: [64]u8 = undefined;
    _ = std.fmt.bufPrint(&hex, "{x}", .{std.fmt.fmtSliceHexLower(&d)}) catch unreachable;
    return hex;
}

test {
    _ = @import("player_test.zig");
}
