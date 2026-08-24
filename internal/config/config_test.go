package config

import "testing"

func fixture() *Fixture {
	return &Fixture{
		OwnedFiles: []string{
			"src/voice/engine.zig",
			"src/voice/range.zig",
		},
		ForbiddenPaths: []string{
			"build.zig",
			"products/**",
			"hidden/**",
		},
	}
}

func TestOwnsAllowsListedFiles(t *testing.T) {
	f := fixture()
	if !f.Owns("src/voice/engine.zig") {
		t.Fatal("an owned file was refused")
	}
	if f.Owns("src/voice/other.zig") {
		t.Fatal("an unlisted file was allowed")
	}
}

func TestOwnsRefusesDirectoryPrefixes(t *testing.T) {
	f := fixture()
	for _, p := range []string{"products/player/golden.zig", "hidden/root.zig", "build.zig"} {
		if f.Owns(p) {
			t.Fatalf("%s was allowed", p)
		}
	}
}

// The two lists must not be able to disagree in the permissive direction: a
// forbidden path stays forbidden even if somebody adds it to the allowlist.
func TestForbiddenBeatsOwned(t *testing.T) {
	f := fixture()
	f.OwnedFiles = append(f.OwnedFiles, "products/player/golden.zig")
	if f.Owns("products/player/golden.zig") {
		t.Fatal("an allowlist entry overrode a forbidden path")
	}
}

func TestOwnsIsNotPrefixMatchingByAccident(t *testing.T) {
	f := fixture()
	// "src/voice/engine.zig" must not admit "src/voice/engine.zig.bak".
	if f.Owns("src/voice/engine.zig.bak") {
		t.Fatal("an exact-path entry matched a longer path")
	}
}
