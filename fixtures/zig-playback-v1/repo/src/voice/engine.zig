//! One playback voice, split into hot and cold state.
//!
//! `Hot` is the audio-plane record and is exactly 72 bytes with fixed field
//! offsets. It is `extern` so the layout is the C ABI's and not the optimizer's.
//! Growing it or reordering it is a breaking change to the audio plane and is
//! refused by tests, so setup-time data belongs in `Cold`.
//!
//! `Cold` is control-plane data captured at note-on. It holds no pointer of any
//! kind: a voice that retained a document, parameter or bank pointer could read
//! it after the control plane replaced it.
//!
//! Per-frame operation order is fixed: reader x envelope x voice gain, then the
//! optional one-pole filter, then declick. Anything that has to be computed from
//! setup state -- the filter coefficient in particular -- is computed once
//! before the frame loop.

const std = @import("std");
const core = @import("core");
const cursor_mod = @import("cursor.zig");

const Cursor = cursor_mod.Cursor;
const SampleData = core.SampleData;
const Segment = core.Segment;

pub const flag_active: u32 = 1 << 0;

/// Largest channel count a voice carries per-channel filter state for.
pub const channels_max: usize = 2;

/// Exactly 72 bytes. Offsets are pinned by tests; see hot_size / hot_offsets.
pub const Hot = extern struct {
    cursor: Cursor, // 0  .. 24
    start_seq: u64, // 24 .. 32
    env_level: f32, // 32 .. 36
    env_rate: f32, // 36 .. 40
    declick_gain: f32, // 40 .. 44
    declick_step: f32, // 44 .. 48
    voice_gain: f32, // 48 .. 52
    filter_coeff: f32, // 52 .. 56
    filter_z1: [channels_max]f32, // 56 .. 64
    flags: u32, // 64 .. 68
    handle: u32, // 68 .. 72
};

pub const hot_size: usize = 72;

comptime {
    if (@sizeOf(Hot) != hot_size) @compileError("Hot must be exactly 72 bytes");
}

pub const Cold = struct {
    segment: Segment,
    base_gain: f32 = 1.0,
};

pub const NoteOnError = error{
    IncoherentSample,
    TooManyChannels,
    BadSegment,
    BadGain,
    BadCoefficient,
    BadRate,
};

/// Legal one-pole coefficients. Exactly 0.0 is a bit-for-bit bypass; 1.0 and
/// above is an unstable pole and is refused rather than clamped.
pub fn coeffIsUsable(c: f32) bool {
    return std.math.isFinite(c) and c >= 0.0 and c < 1.0;
}

pub fn gainIsUsable(g: f32) bool {
    return std.math.isFinite(g) and g >= 0.0;
}

pub const Voice = struct {
    hot: Hot,
    cold: Cold,

    pub fn idle() Voice {
        return .{
            .hot = .{
                .cursor = .{ .frame = 0, .phase = 0, .rate = 0 },
                .start_seq = 0,
                .env_level = 0,
                .env_rate = 0,
                .declick_gain = 0,
                .declick_step = 0,
                .voice_gain = 0,
                .filter_coeff = 0,
                .filter_z1 = .{ 0, 0 },
                .flags = 0,
                .handle = 0,
            },
            .cold = .{ .segment = .{ .start = 0, .frames = 0 } },
        };
    }

    pub fn isActive(self: *const Voice) bool {
        return self.hot.flags & flag_active != 0;
    }

    /// Capture setup state and resolve everything the frame loop will need.
    ///
    /// Every refusal here is by name and leaves the voice inactive; there is no
    /// permissive canonicalization of a bad range, gain or coefficient.
    pub fn noteOn(
        self: *Voice,
        s: *const SampleData,
        cold: Cold,
        rate: f64,
        seq: u64,
        handle: u32,
    ) NoteOnError!void {
        if (!s.isCoherent()) return error.IncoherentSample;
        if (s.channels > channels_max) return error.TooManyChannels;
        if (!Cursor.rateIsUsable(rate)) return error.BadRate;
        if (!gainIsUsable(cold.base_gain)) return error.BadGain;
        if (!coeffIsUsable(cold.segment.filter_coeff)) return error.BadCoefficient;

        const bounds = try boundsOf(&cold, s);

        const first: u64 = if (rate < 0.0) bounds[1] - 1 else bounds[0];
        self.cold = cold;
        self.hot = .{
            .cursor = Cursor.start(first, rate),
            .start_seq = seq,
            .env_level = 1.0,
            .env_rate = 0.0,
            .declick_gain = 1.0,
            .declick_step = 0.0,
            .voice_gain = cold.base_gain,
            .filter_coeff = resolvedCoeff(&cold),
            .filter_z1 = .{ 0, 0 },
            .flags = flag_active,
            .handle = handle,
        };
    }

    /// Playable half-open bounds [lo, hi) for this voice, resolved from cold
    /// state once. HEAD plays the whole sample.
    fn boundsOf(cold: *const Cold, s: *const SampleData) NoteOnError![2]u64 {
        _ = cold;
        return .{ 0, s.frames };
    }

    /// The one-pole coefficient the frame loop will use, resolved from cold
    /// state. HEAD inherits the segment's coefficient unconditionally.
    fn resolvedCoeff(cold: *const Cold) f32 {
        return cold.segment.filter_coeff;
    }

    /// Render into `out`, one planar slice per channel, all the same length.
    /// Returns the number of frames written; a short return means the voice hit
    /// the end of its playable bounds and retired.
    pub fn renderBlock(self: *Voice, s: *const SampleData, out: []const []f32) usize {
        std.debug.assert(out.len == s.channels);
        std.debug.assert(self.isActive());

        // Everything derived from setup state is computed here, before the frame
        // loop. Nothing inside the loop may consult cold state or call a
        // transcendental, an allocator or a lock.
        const bounds = boundsOf(&self.cold, s) catch unreachable;
        const lo = bounds[0];
        const hi = bounds[1];
        const coeff = self.hot.filter_coeff;
        const n = out[0].len;

        var written: usize = 0;
        while (written < n) {
            const t = self.hot.cursor.taps(lo, hi);
            const frac: f32 = @floatCast(self.hot.cursor.phase);
            for (out, 0..) |plane, ch| {
                const a = s.tap(ch, t[0]);
                const b = s.tap(ch, t[1]);
                var v = a + (b - a) * frac;
                v *= self.hot.env_level;
                v *= self.hot.voice_gain;
                const y = v + coeff * (self.hot.filter_z1[ch] - v);
                self.hot.filter_z1[ch] = y;
                plane[written] = y * self.hot.declick_gain;
            }
            written += 1;
            self.stepEnvelope();
            self.hot.cursor.advance(lo, hi) catch {
                self.hot.flags &= ~flag_active;
                break;
            };
        }
        return written;
    }

    fn stepEnvelope(self: *Voice) void {
        self.hot.env_level = @min(1.0, self.hot.env_level + self.hot.env_rate);
        self.hot.declick_gain = @min(1.0, self.hot.declick_gain + self.hot.declick_step);
    }
};
