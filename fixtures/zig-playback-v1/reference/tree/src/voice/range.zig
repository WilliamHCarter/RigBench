//! Half-open frame intervals.
//!
//! Every frame interval in this repository is `[start, end)`. There is one
//! validity rule and it lives here, so a consumer cannot invent a second one.

pub const FrameRange = struct {
    start: u64,
    end: u64,

    pub fn full(frames: u64) FrameRange {
        return .{ .start = 0, .end = frames };
    }

    /// A range is valid iff it is non-empty and lies inside the sample.
    /// `end == frames` is legal; `end` itself is never read.
    pub fn validate(self: FrameRange, frames: u64) bool {
        return self.start < self.end and self.end <= frames;
    }

    /// Frame count. Only meaningful for a validated range.
    pub fn len(self: FrameRange) u64 {
        return self.end - self.start;
    }
};
