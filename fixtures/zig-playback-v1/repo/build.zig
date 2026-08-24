const std = @import("std");

// Frozen fixture build graph. AgentBench-01 owns these step names; a bounded
// builder task must not edit this file. `test` runs the visible suite that ships
// with the repository. `test-hidden` runs whatever `hidden/root.zig` contains,
// which at HEAD is an empty placeholder and at scoring time is replaced by the
// benchmark's hidden invariant suite.
pub fn build(b: *std.Build) void {
    const target = b.standardTargetOptions(.{});
    const optimize = b.standardOptimizeOption(.{});

    const core = b.addModule("core", .{
        .root_source_file = b.path("src/core/root.zig"),
        .target = target,
        .optimize = optimize,
    });

    const voice = b.addModule("voice", .{
        .root_source_file = b.path("src/voice/root.zig"),
        .target = target,
        .optimize = optimize,
        .imports = &.{.{ .name = "core", .module = core }},
    });

    const player = b.addModule("player", .{
        .root_source_file = b.path("products/player/player.zig"),
        .target = target,
        .optimize = optimize,
        .imports = &.{
            .{ .name = "core", .module = core },
            .{ .name = "voice", .module = voice },
        },
    });

    const test_step = b.step("test", "Run the visible test suite");
    for ([_]*std.Build.Module{ core, voice, player }) |mod| {
        const t = b.addTest(.{ .root_module = mod });
        test_step.dependOn(&b.addRunArtifact(t).step);
    }

    // Hidden suite. Its root module can reach every source file in the tree,
    // including the raw bytes of src/voice/engine.zig for structural controls.
    const hidden = b.createModule(.{
        .root_source_file = b.path("hidden/root.zig"),
        .target = target,
        .optimize = optimize,
        .imports = &.{
            .{ .name = "core", .module = core },
            .{ .name = "voice", .module = voice },
            .{ .name = "player", .module = player },
        },
    });
    // Raw source bytes, so a hidden structural control can assert *where* a
    // computation happens -- something no ordinary test can observe.
    hidden.addAnonymousImport("engine_src", .{ .root_source_file = b.path("src/voice/engine.zig") });
    hidden.addAnonymousImport("cursor_src", .{ .root_source_file = b.path("src/voice/cursor.zig") });

    const hidden_tests = b.addTest(.{ .root_module = hidden });
    const hidden_step = b.step("test-hidden", "Run the injected hidden suite");
    hidden_step.dependOn(&b.addRunArtifact(hidden_tests).step);

    // Release-mode rung: an invariant whose only witness is a Debug safety
    // check is not a witness for the mode this code ships in. The whole graph is
    // rebuilt in ReleaseFast rather than mixing optimization modes.
    const release_step = b.step("test-release", "Run the visible suite in ReleaseFast");
    const core_rf = b.createModule(.{
        .root_source_file = b.path("src/core/root.zig"),
        .target = target,
        .optimize = .ReleaseFast,
    });
    const voice_rf = b.createModule(.{
        .root_source_file = b.path("src/voice/root.zig"),
        .target = target,
        .optimize = .ReleaseFast,
        .imports = &.{.{ .name = "core", .module = core_rf }},
    });
    const player_rf = b.createModule(.{
        .root_source_file = b.path("products/player/player.zig"),
        .target = target,
        .optimize = .ReleaseFast,
        .imports = &.{
            .{ .name = "core", .module = core_rf },
            .{ .name = "voice", .module = voice_rf },
        },
    });
    for ([_]*std.Build.Module{ core_rf, voice_rf, player_rf }) |mod| {
        const t = b.addTest(.{ .root_module = mod });
        release_step.dependOn(&b.addRunArtifact(t).step);
    }
}
