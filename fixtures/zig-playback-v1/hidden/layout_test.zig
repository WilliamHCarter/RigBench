//! Layout and reflection controls.
//!
//! These do not call the engine. They ask the type system what the engine
//! *is*, which is the only way to catch a seam that was implemented by growing
//! the audio-plane record.

const std = @import("std");
const voice = @import("voice");

const Hot = voice.Hot;
const Cold = voice.Cold;

/// The frozen audio-plane record: exact field names, in order, at exact
/// offsets, with exact types. A new field cannot hide here even if it were
/// somehow free, because the field *count* is pinned too.
const HotField = struct { name: []const u8, offset: usize, type_name: []const u8 };

const hot_frozen = [_]HotField{
    .{ .name = "cursor", .offset = 0, .type_name = "Cursor" },
    .{ .name = "start_seq", .offset = 24, .type_name = "u64" },
    .{ .name = "env_level", .offset = 32, .type_name = "f32" },
    .{ .name = "env_rate", .offset = 36, .type_name = "f32" },
    .{ .name = "declick_gain", .offset = 40, .type_name = "f32" },
    .{ .name = "declick_step", .offset = 44, .type_name = "f32" },
    .{ .name = "voice_gain", .offset = 48, .type_name = "f32" },
    .{ .name = "filter_coeff", .offset = 52, .type_name = "f32" },
    .{ .name = "filter_z1", .offset = 56, .type_name = "[2]f32" },
    .{ .name = "flags", .offset = 64, .type_name = "u32" },
    .{ .name = "handle", .offset = 68, .type_name = "u32" },
};

test "Hot is exactly 72 bytes and no larger" {
    try std.testing.expectEqual(@as(usize, 72), @sizeOf(Hot));
}

test "Hot is extern, so its layout is the ABI's and not the optimizer's" {
    const info = @typeInfo(Hot).@"struct";
    try std.testing.expectEqual(std.builtin.Type.ContainerLayout.@"extern", info.layout);
}

test "Hot has exactly the frozen field set, in order, at the frozen offsets" {
    const fields = @typeInfo(Hot).@"struct".fields;

    // Compared over the overlap first, so a field-count change reports as a
    // field-count change. Indexing the frozen list by a field index would be a
    // comptime bounds error, which is a correct refusal with a useless message.
    inline for (0..@min(fields.len, hot_frozen.len)) |i| {
        const want = hot_frozen[i];
        const f = fields[i];
        try std.testing.expectEqualStrings(want.name, f.name);
        try std.testing.expectEqual(want.offset, @offsetOf(Hot, f.name));
    }

    if (fields.len != hot_frozen.len) {
        std.debug.print("\nHot has {d} fields, want exactly {d}. Extra or missing:\n",
            .{ fields.len, hot_frozen.len });
        inline for (fields, 0..) |f, i| {
            if (i >= hot_frozen.len) {
                std.debug.print("  + {s}: {s} at offset {d}\n",
                    .{ f.name, @typeName(f.type), @offsetOf(Hot, f.name) });
            }
        }
        inline for (hot_frozen, 0..) |want, i| {
            if (i >= fields.len) std.debug.print("  - {s}\n", .{want.name});
        }
        return error.HotFieldSetChanged;
    }
}

test "no Hot field is a range or a tone" {
    const fields = @typeInfo(Hot).@"struct".fields;
    inline for (fields) |f| {
        const name = @typeName(f.type);
        try std.testing.expect(std.mem.indexOf(u8, name, "Tone") == null);
        try std.testing.expect(std.mem.indexOf(u8, name, "FrameRange") == null);
        try std.testing.expect(std.mem.indexOf(u8, name, "Filter") == null);
    }
}

test "Cold retains no pointer, at any depth" {
    try expectNoPointer(Cold);
}

fn expectNoPointer(comptime T: type) !void {
    switch (@typeInfo(T)) {
        .pointer => return error.PointerRetained,
        .optional => |o| try expectNoPointer(o.child),
        .array => |a| try expectNoPointer(a.child),
        .@"struct" => |s| inline for (s.fields) |f| try expectNoPointer(f.type),
        .@"union" => |u| inline for (u.fields) |f| try expectNoPointer(f.type),
        else => {},
    }
}

test "Cold carries the seam as cold state" {
    const fields = @typeInfo(Cold).@"struct".fields;
    var saw_range = false;
    var saw_tone = false;
    inline for (fields) |f| {
        if (std.mem.eql(u8, f.name, "range")) saw_range = true;
        if (std.mem.eql(u8, f.name, "tone")) saw_tone = true;
    }
    try std.testing.expect(saw_range);
    try std.testing.expect(saw_tone);
}

test "the seam's Cold fields default to the pre-seam meaning" {
    const c = Cold{ .segment = .{ .start = 0, .frames = 8 } };
    try std.testing.expect(c.range == null);
    try std.testing.expectEqual(@as(f32, 1.0), c.tone.gain);
    try std.testing.expectEqual(voice.Filter.inherit_segment, c.tone.filter);
}

test "Filter is a tagged union with exactly the three declared cases" {
    const info = @typeInfo(voice.Filter).@"union";
    try std.testing.expect(info.tag_type != null);
    try std.testing.expectEqual(@as(usize, 3), info.fields.len);
    try std.testing.expectEqualStrings("inherit_segment", info.fields[0].name);
    try std.testing.expectEqualStrings("bypass", info.fields[1].name);
    try std.testing.expectEqualStrings("coefficient", info.fields[2].name);
    try std.testing.expectEqual(f32, info.fields[2].type);
}

test "FrameRange is two u64 and nothing else" {
    const fields = @typeInfo(voice.FrameRange).@"struct".fields;
    try std.testing.expectEqual(@as(usize, 2), fields.len);
    try std.testing.expectEqualStrings("start", fields[0].name);
    try std.testing.expectEqualStrings("end", fields[1].name);
    try std.testing.expectEqual(u64, fields[0].type);
    try std.testing.expectEqual(u64, fields[1].type);
}

test "Cursor is still 24 bytes at offset zero of Hot" {
    try std.testing.expectEqual(@as(usize, 24), @sizeOf(voice.Cursor));
    try std.testing.expectEqual(@as(usize, 0), @offsetOf(Hot, "cursor"));
}
