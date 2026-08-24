//! `voice` module root. Every file under src/voice/ is re-exported here and
//! referenced inside `test {}`; a module reachable by neither is not built.
pub const cursor = @import("cursor.zig");
pub const engine = @import("engine.zig");

pub const Cursor = cursor.Cursor;
pub const Mode = cursor.Mode;
pub const AdvanceError = cursor.AdvanceError;
pub const Hot = engine.Hot;
pub const Cold = engine.Cold;
pub const Voice = engine.Voice;
pub const hot_size = engine.hot_size;
pub const channels_max = engine.channels_max;
pub const flag_active = engine.flag_active;

test {
    _ = cursor;
    _ = engine;
    _ = @import("cursor_test.zig");
    _ = @import("engine_test.zig");
}
