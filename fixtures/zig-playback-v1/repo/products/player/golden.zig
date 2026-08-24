//! The frozen Player golden.
//!
//! Not owned by a bounded voice-engine task. If a change moves these bytes,
//! the change is not default-compatible, and blessing this file is not the
//! repair.
pub const reference_digest_hex: *const [64:0]u8 = "cbedbfc4ac127447943d8a4cb3e9acc80fbb06c68640cde5a6cc20f9e89ad8f9";

/// The first eight output frames of the left plane, as raw f32 bit patterns.
/// A digest tells you *that* something moved; these tell you where to look.
pub const reference_head_bits: [8]u32 = .{
    0xbf100000, 0xbf065500, 0xbeac7e80, 0xbde47680,
    0x3dfc4260, 0x3eb87226, 0x3d11044c, 0xbedc1dde,
};

pub const reference_written: usize = 86;
