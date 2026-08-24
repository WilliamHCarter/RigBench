package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/WilliamHCarter/RigBench/internal/config"
	"github.com/WilliamHCarter/RigBench/internal/metrics"
	"github.com/WilliamHCarter/RigBench/internal/mock"
	"github.com/WilliamHCarter/RigBench/internal/prompt"
	"github.com/WilliamHCarter/RigBench/internal/report"
	"github.com/WilliamHCarter/RigBench/internal/runner"
)

// v02Options configures the v0.2 acceptance gate.
type v02Options struct {
	fixtureDir string
	engines    string
	layouts    []string
	runsDir    string
	root       string
	timeScale  float64
}

// acceptV02 runs the v0.2 acceptance gate:
//
//   - turn N's prompt is byte-identical to turn N-1's prefix plus appended data;
//   - a repeated run produces identical prompt hashes;
//   - cold T0 and warm T1..T3 are reported separately;
//   - the prefix-friendly layout can be A/B tested without changing task
//     semantics, and its reusable prefix is measurably larger.
func acceptV02(ctx context.Context, o v02Options) ([]report.LayoutRow, error) {
	f, err := config.LoadFixture(o.fixtureDir)
	if err != nil {
		return nil, err
	}
	traj, err := f.LoadTrajectory()
	if err != nil {
		return nil, err
	}
	tok := prompt.Approx{}

	fmt.Printf("trajectory %s: %d turns, scored turn %d\n\n", traj.ID, len(traj.Turns), traj.ScoredTurn)

	// --- 1. append-only growth, and byte reproducibility -------------------
	fmt.Println("== append-only prompt growth ==")
	manifests := map[string][]*prompt.Manifest{}
	for _, lp := range o.layouts {
		layout, err := config.LoadLayout(lp)
		if err != nil {
			return nil, err
		}
		build := func() ([]*prompt.Manifest, error) {
			var out []*prompt.Manifest
			for i := range traj.Turns {
				m, err := runner.BuildPrompt(runner.Options{
					Fixture: f, Layout: layout, ContextPack: "base",
					Tokenizer: tok, Trajectory: traj,
					RunID: "acceptance", RunDir: o.root, WorkDir: o.root,
				}, i)
				if err != nil {
					return nil, fmt.Errorf("layout %s turn %d: %w", layout.ID, i, err)
				}
				out = append(out, m)
			}
			return out, nil
		}

		first, err := build()
		if err != nil {
			return nil, err
		}
		for i := 1; i < len(first); i++ {
			if err := first[i].AppendsOnto(first[i-1]); err != nil {
				return nil, fmt.Errorf("layout %s: %w", layout.ID, err)
			}
		}
		fmt.Printf("  %-26s every turn is a byte-exact append of the one before it\n", layout.ID)

		// Byte reproducibility: build the whole trajectory again from scratch
		// and require identical hashes. A serializer that reached for a clock,
		// a map iteration order or a temp path fails here.
		second, err := build()
		if err != nil {
			return nil, err
		}
		for i := range first {
			if first[i].PromptSHA256 != second[i].PromptSHA256 {
				return nil, fmt.Errorf("layout %s turn %d is not byte-reproducible: %s vs %s",
					layout.ID, i, first[i].PromptSHA256[:12], second[i].PromptSHA256[:12])
			}
		}
		fmt.Printf("  %-26s a repeated build produces identical prompt hashes\n", layout.ID)

		for i, m := range first {
			added := 0
			if i > 0 {
				added = m.PromptBytes - first[i-1].PromptBytes
			}
			fmt.Printf("      t%d  prompt %6d B  (+%5d)  reusable prefix %6d B  %3.0f%%  sha %s\n",
				i, m.PromptBytes, added, m.StablePrefixBytes,
				100*float64(m.StablePrefixBytes)/float64(m.PromptBytes), m.PromptSHA256[:12])
		}
		manifests[layout.ID] = first
		fmt.Println()
	}

	// --- 2. the layouts differ in order only ------------------------------
	fmt.Println("== layout A/B carries identical bytes ==")
	ids := make([]string, 0, len(manifests))
	for id := range manifests {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) < 2 {
		return nil, fmt.Errorf("the layout A/B needs at least two layouts, got %d", len(ids))
	}
	base := manifests[ids[0]]
	for _, id := range ids[1:] {
		other := manifests[id]
		if len(other) != len(base) {
			return nil, fmt.Errorf("layouts %s and %s produced different turn counts", ids[0], id)
		}
		for i := range base {
			if err := prompt.SameContent(base[i], other[i]); err != nil {
				return nil, fmt.Errorf("turn %d: %w", i, err)
			}
		}
		fmt.Printf("  %-26s same block bytes as %s at every turn; only order differs\n", id, ids[0])
	}

	// --- 3. and the cache-friendly one has the larger reusable prefix ------
	fmt.Println("\n== reusable prefix by layout ==")
	var rows []report.LayoutRow
	for _, id := range ids {
		last := manifests[id][len(manifests[id])-1]
		share := float64(last.StablePrefixBytes) / float64(last.PromptBytes)
		rows = append(rows, report.LayoutRow{
			Layout: id, StablePrefixBytes: last.StablePrefixBytes, Share: share,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].StablePrefixBytes > rows[j].StablePrefixBytes })
	rows[0].Note = "largest reusable prefix"
	for i := 1; i < len(rows); i++ {
		rows[i].Note = fmt.Sprintf("%d bytes less reusable than `%s`",
			rows[0].StablePrefixBytes-rows[i].StablePrefixBytes, rows[0].Layout)
	}
	for _, r := range rows {
		fmt.Printf("  %-26s %6d B  %3.0f%% of the final prompt  %s\n",
			r.Layout, r.StablePrefixBytes, 100*r.Share, r.Note)
	}
	if rows[0].StablePrefixBytes <= rows[len(rows)-1].StablePrefixBytes {
		return nil, fmt.Errorf("no layout has a larger reusable prefix than another; " +
			"the A/B cannot show anything")
	}
	if !strings.Contains(rows[0].Layout, "cache-friendly") {
		return nil, fmt.Errorf("the layout with the largest reusable prefix is %q, "+
			"which is not the cache-friendly one; either the layouts or the "+
			"serializer disagree with the design rule", rows[0].Layout)
	}

	// --- 3b. what the layout difference actually buys ---------------------
	//
	// Within one story both layouts append cleanly, because the leading
	// objective of a `current`-layout prompt does not change between turns of
	// the same task. So turn-to-turn reuse is *not* where the layouts differ,
	// and saying otherwise would be the easy overclaim here.
	//
	// The difference is across tasks. A second story reuses a cache-friendly
	// layout's whole stable head and reuses essentially nothing of a
	// volatile-first one. That is measured directly below, over the serializer's
	// own bytes.
	fmt.Println("\n== cross-task reuse ==")
	fmt.Println("  Two different objectives over identical doctrine, story and source context.")
	fmt.Println("  Reuse is computed at 256-byte block granularity, the way a KV cache reuses.")
	for _, lp := range o.layouts {
		layout, err := config.LoadLayout(lp)
		if err != nil {
			return nil, err
		}
		mkPrompt := func(traj *config.Trajectory) (string, error) {
			m, err := runner.BuildPrompt(runner.Options{
				Fixture: f, Layout: layout, ContextPack: "base",
				Tokenizer: tok, Trajectory: traj,
				RunID: "acceptance", RunDir: o.root, WorkDir: o.root,
			}, 0)
			if err != nil {
				return "", err
			}
			return prompt.Canonical(m.Messages), nil
		}
		// Task A uses the trajectory's turn-0 objective; task B uses the
		// fixture's standalone single-turn objective. Same stable material,
		// different volatile tail.
		taskA, err := mkPrompt(traj)
		if err != nil {
			return nil, err
		}
		taskB, err := mkPrompt(nil)
		if err != nil {
			return nil, err
		}
		cache := mock.NewPrefixCache(4)
		cache.Observe(taskA, 256)
		reused := cache.Observe(taskB, 256)
		fmt.Printf("  %-26s %6d of %6d bytes reusable on the second task  %3.0f%%\n",
			layout.ID, reused, len(taskB), 100*float64(reused)/float64(len(taskB)))
		for i := range rows {
			if rows[i].Layout == layout.ID {
				rows[i].CrossTaskReuseBytes = reused
			}
		}
	}
	best, worst := rows[0], rows[len(rows)-1]
	if best.CrossTaskReuseBytes <= worst.CrossTaskReuseBytes {
		return nil, fmt.Errorf("the cache-friendly layout does not reuse more across tasks "+
			"(%d vs %d bytes); the A/B shows nothing",
			best.CrossTaskReuseBytes, worst.CrossTaskReuseBytes)
	}

	// --- 4. a live multi-turn run, cold T0 and warm T1..TN separated -------
	fmt.Println("\n== multi-turn run, cold and warm reported separately ==")
	stage, err := os.MkdirTemp("", "agentbench-v02-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	body, err := mock.BuildResponse(ctx, f, mock.Reference, filepath.Join(stage, "canned"))
	if err != nil {
		return nil, err
	}

	for _, lp := range o.layouts {
		layout, err := config.LoadLayout(lp)
		if err != nil {
			return nil, err
		}
		// A fresh cache per layout: a cold run must start from a cache that
		// genuinely holds nothing.
		cache := mock.NewPrefixCache(64)
		srv := &mock.Server{
			TimeScale: o.timeScale, ProfileFor: profileFromRequest,
			Respond:         func(int) (string, string) { return body, "" },
			Cache:           cache,
			CacheBlockBytes: 256,
		}
		ln, shutdown, err := srv.Listen("127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		runDir, err := doRun(ctx, &runFlags{
			fixtureDir:  o.fixtureDir,
			engines:     o.engines,
			layout:      lp,
			contextPack: "base",
			thermal:     "cold",
			runsDir:     o.root,
			runID:       "v02-" + shortHash(layout.ID),
			runSubdir:   "turns-" + layout.ID,
			endpoint:    fmt.Sprintf("http://%s/v1", ln.Addr()),
			repeats:     1,
			turns:       true,
			// Both engines share one mock process, so without this the second
			// engine's "cold" turn 0 would hit a cache the first engine warmed
			// -- a thermal label that is simply false.
			preEngine: func(string) error { cache.Reset(); return nil },
			caveats: []string{
				fmt.Sprintf("Endpoint was the in-repo deterministic mock with a **simulated** "+
					"prefix cache (time-scale %g). The cache hit counts below model what a "+
					"served cache could reuse; they are not a measurement of one.", o.timeScale),
			},
			layoutRows: rows,
		})
		_ = shutdown(ctx)
		if err != nil {
			return nil, err
		}

		recs, err := metrics.ReadRecords(filepath.Join(runDir, "request.jsonl"))
		if err != nil {
			return nil, err
		}
		if err := checkThermalSeparation(recs, len(traj.Turns)); err != nil {
			return nil, fmt.Errorf("layout %s: %w", layout.ID, err)
		}
		if err := checkScoring(recs, traj.ScoredTurn); err != nil {
			return nil, fmt.Errorf("layout %s: %w", layout.ID, err)
		}
		fmt.Printf("  %-26s cold t0 and warm-resident t1..t%d are separate cells; "+
			"only t%d carries a verdict\n",
			layout.ID, len(traj.Turns)-1, traj.ScoredTurn)
	}

	return rows, nil
}

// checkThermalSeparation requires turn 0 of a cold run to be recorded cold and
// every later turn warm-resident, and requires the reporter to keep them apart.
func checkThermalSeparation(recs []metrics.Record, turns int) error {
	seen := map[string]int{}
	for _, r := range recs {
		want := "warm-resident"
		if r.TurnIndex == 0 {
			want = "cold"
		}
		if r.Thermal != want {
			return fmt.Errorf("turn %d recorded thermal %q, want %q", r.TurnIndex, r.Thermal, want)
		}
		seen[r.Thermal]++
	}
	if seen["cold"] == 0 || seen["warm-resident"] == 0 {
		return fmt.Errorf("a %d-turn cold run produced no cold/warm split: %v", turns, seen)
	}
	for _, c := range report.Aggregate(recs) {
		for _, r := range recs {
			if r.Thermal != c.Key.Thermal {
				continue
			}
			if (r.TurnIndex == 0) != (c.Key.Thermal == "cold") {
				return fmt.Errorf("cell %s mixes turn %d into the wrong thermal class",
					c.Key, r.TurnIndex)
			}
		}
	}
	return nil
}

// checkScoring requires exactly the declared turn to carry a verdict.
func checkScoring(recs []metrics.Record, scoredTurn int) error {
	for _, r := range recs {
		want := r.TurnIndex == scoredTurn
		if r.Scored != want {
			return fmt.Errorf("turn %d has scored=%v, want %v", r.TurnIndex, r.Scored, want)
		}
		if want && (r.Quality == nil || !r.Quality.Passed) {
			return fmt.Errorf("the scored turn did not pass its gates: %v",
				strings.Join(qualityFailNames(r.Quality), ", "))
		}
		if !want && r.Quality != nil {
			return fmt.Errorf("unscored turn %d carries a quality verdict", r.TurnIndex)
		}
	}
	return nil
}

func qualityFailNames(q *metrics.Quality) []string {
	if q == nil {
		return []string{"no quality record at all"}
	}
	return q.FailedGates()
}
