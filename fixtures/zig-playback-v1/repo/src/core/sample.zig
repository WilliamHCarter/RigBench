//! Decoded sample payloads and the segment record a voice is started from.
//!
//! Ownership: a `SampleData` is control-plane owned and immutable once
//! published. A voice may hold `*const SampleData` and never a mutable pointer.

/// Guard frames written at both ends of every plane. A tap at audio-relative
/// index `frames` therefore lands in the guard and reads exact zero rather than
/// out of bounds; range legality is enforced by the cursor, not by the pads.
pub const pad_frames: u32 = 16;

pub const Coherence = error{Incoherent};

pub const SampleData = struct {
    channels: u16,
    frames: u64,
    sample_rate: u32,
    pad: u32,
    /// One plane per channel, each exactly `pad + frames + pad` long.
    planes: []const []const f32,

    /// Audio-relative tap. `index` is measured from the first audio frame, so
    /// `0` is the first real sample and `frames` is the first guard frame.
    pub fn tap(self: *const SampleData, ch: usize, index: u64) f32 {
        return self.planes[ch][@intCast(@as(u64, self.pad) + index)];
    }

    pub fn planeLen(self: *const SampleData) u64 {
        return @as(u64, self.pad) + self.frames + @as(u64, self.pad);
    }

    /// Total over the closed set of shape rules. Every arm has a negative
    /// witness in sample_test.zig; `return true` fails that file.
    pub fn isCoherent(self: *const SampleData) bool {
        if (self.channels == 0) return false;
        if (self.frames == 0) return false;
        if (self.sample_rate == 0) return false;
        if (self.pad != pad_frames) return false;
        if (self.planes.len != self.channels) return false;
        const want = self.planeLen();
        for (self.planes) |plane| {
            if (plane.len != want) return false;
            for (plane[0..self.pad]) |v| if (@as(u32, @bitCast(v)) != 0) return false;
            for (plane[plane.len - self.pad ..]) |v| if (@as(u32, @bitCast(v)) != 0) return false;
        }
        return true;
    }
};

/// The slice of a sample a voice was started on, plus the segment-level filter
/// coefficient a voice inherits when it does not override it.
pub const Segment = struct {
    start: u64,
    frames: u64,
    /// One-pole coefficient in [0, 1). Exactly 0.0 is a bit-for-bit bypass.
    filter_coeff: f32 = 0.0,
};
