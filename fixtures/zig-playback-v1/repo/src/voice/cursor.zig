//! Read position for one voice.
//!
//! Two rules the rest of the engine depends on:
//!
//!   1. Playback bounds are half-open. Forward playback stops *before* reading
//!      the upper bound; reverse playback never reads below the lower bound.
//!   2. `advance` decides whether the next source access is legal *before* it
//!      commits any cursor state. A refused advance leaves `frame` and `phase`
//!      exactly as they were. There is no partial mutation and no clamp.
//!
//! Direction is carried by the sign of `rate`; there is no separate mode field,
//! because a second source of truth for direction can disagree with the first.

const std = @import("std");

pub const Mode = enum { forward, reverse };

pub const AdvanceError = error{
    /// The next position would leave the playable bounds. Cursor unchanged.
    Exhausted,
    /// Rate is zero, subnormal-to-zero, or non-finite. Cursor unchanged.
    BadRate,
};

/// 24 bytes, three naturally aligned fields. `Hot` embeds this at offset 0 and
/// the layout is pinned by tests, so this is `extern` rather than `struct`.
pub const Cursor = extern struct {
    frame: u64,
    phase: f64,
    rate: f64,

    pub fn start(frame: u64, rate: f64) Cursor {
        return .{ .frame = frame, .phase = 0.0, .rate = rate };
    }

    pub fn mode(self: Cursor) Mode {
        return if (self.rate < 0.0) .reverse else .forward;
    }

    pub fn rateIsUsable(rate: f64) bool {
        return std.math.isFinite(rate) and rate != 0.0 and @abs(rate) <= 64.0;
    }

    /// `frame` inside [lo, hi), `phase` inside [0, 1), rate usable.
    pub fn isCoherent(self: Cursor, lo: u64, hi: u64) bool {
        if (!rateIsUsable(self.rate)) return false;
        if (!(self.phase >= 0.0 and self.phase < 1.0)) return false;
        if (self.frame < lo) return false;
        if (self.frame >= hi) return false;
        return true;
    }

    /// The two audio-relative taps a linear read consumes, both clamped inside
    /// [lo, hi). The upper tap is clamped to `hi - 1`, so no legal cursor can
    /// produce a read at `hi`.
    pub fn taps(self: Cursor, lo: u64, hi: u64) [2]u64 {
        const lower = @max(lo, @min(self.frame, hi - 1));
        const upper = @min(lower + 1, hi - 1);
        return .{ lower, upper };
    }

    /// Step one output frame inside the half-open bounds [lo, hi).
    ///
    /// Legality is computed on a candidate and only then written back. Callers
    /// treat `Exhausted` as retirement, never as a clamp.
    pub fn advance(self: *Cursor, lo: u64, hi: u64) AdvanceError!void {
        if (!rateIsUsable(self.rate)) return error.BadRate;

        const moved = self.phase + self.rate;
        const carry = @floor(moved);
        const next_phase = moved - carry;

        var next_frame: u64 = undefined;
        if (carry >= 0.0) {
            const step: u64 = @intFromFloat(carry);
            const sum = @addWithOverflow(self.frame, step);
            if (sum[1] != 0) return error.Exhausted;
            next_frame = sum[0];
        } else {
            const step: u64 = @intFromFloat(-carry);
            if (step > self.frame) return error.Exhausted;
            next_frame = self.frame - step;
        }

        if (next_frame < lo or next_frame >= hi) return error.Exhausted;

        self.frame = next_frame;
        self.phase = next_phase;
    }
};
