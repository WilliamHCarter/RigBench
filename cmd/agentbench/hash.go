package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/WilliamHCarter/RigBench/internal/config"
	"github.com/WilliamHCarter/RigBench/internal/prompt"
)

// hashFixture digests every byte the benchmark's prompts and gates are built
// from, and returns a single manifest digest over the sorted per-file digests.
//
// Freezing a release means freezing this number. Build artifacts are excluded
// because they are derived; everything else is included, so a change to a
// hidden test or a mutant definition moves the manifest hash and marks the
// results incomparable.
func hashFixture(f *config.Fixture) (string, map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(f.Dir(), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(f.Dir(), path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		for _, seg := range strings.Split(rel, "/") {
			if seg == ".zig-cache" || seg == "zig-out" || seg == ".git" {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		// Hashed after the same normalization the serializer applies, so a
		// line-ending change on checkout does not read as a fixture change.
		sum := sha256.Sum256([]byte(prompt.Normalize(string(b))))
		files[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		return "", nil, err
	}

	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(files[k]))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil)), files, nil
}

// zigPin reads the compiler version the fixture pins. Under anyzig this is the
// single source of truth for which compiler will run, so it is read from the
// file rather than from a global `zig version`.
func zigPin(f *config.Fixture) string {
	b, err := os.ReadFile(filepath.Join(f.Path(f.RepoDir), "build.zig.zon"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, ".minimum_zig_version") {
			if i := strings.Index(line, "\""); i >= 0 {
				if j := strings.Index(line[i+1:], "\""); j >= 0 {
					return line[i+1 : i+1+j]
				}
			}
		}
	}
	return ""
}
