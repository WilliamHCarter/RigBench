//! Per-voice gain and filter override, captured at note-on.
//!
//! `Tone` is cold state. Nothing here is reachable from the frame loop: the
//! engine calls `resolveCoeff` once at note-on and stores the answer in the
//! `Hot.filter_coeff` field that already exists.

const std = @import("std");

pub const ToneError = error{
    BadGain,
    BadCoefficient,
};

pub const Filter = union(enum) {
    /// Use whatever coefficient the segment carries.
    inherit_segment,
    /// Exactly 0.0, which the one-pole `y = x + c*(z1 - x)` treats as a
    /// bit-for-bit bypass. This is a bypass by arithmetic identity, not by a
    /// branch the frame loop has to take.
    bypass,
    /// An explicit coefficient, valid in [0, 1).
    coefficient: f32,
};

pub const Tone = struct {
    gain: f32 = 1.0,
    filter: Filter = .inherit_segment,

    pub fn gainIsUsable(g: f32) bool {
        return std.math.isFinite(g) and g >= 0.0;
    }

    pub fn coeffIsUsable(c: f32) bool {
        return std.math.isFinite(c) and c >= 0.0 and c < 1.0;
    }

    /// Total over the three filter cases. Refuses rather than canonicalizing:
    /// there is no clamp of an out-of-domain coefficient and no substitution of
    /// a default for a NaN.
    pub fn resolveCoeff(self: Tone, segment_coeff: f32) ToneError!f32 {
        if (!gainIsUsable(self.gain)) return error.BadGain;
        return switch (self.filter) {
            .inherit_segment => if (coeffIsUsable(segment_coeff))
                segment_coeff
            else
                error.BadCoefficient,
            .bypass => 0.0,
            .coefficient => |c| if (coeffIsUsable(c)) c else error.BadCoefficient,
        };
    }
};
