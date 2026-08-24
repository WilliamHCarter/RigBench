package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/WilliamHCarter/RigBench/internal/config"
	"github.com/WilliamHCarter/RigBench/internal/metrics"
	"github.com/WilliamHCarter/RigBench/internal/mock"
)

// acceptNoThink proves that a config claiming "thinking: off" produces a
// request that actually disables thinking, and that the proof is not vacuous.
//
// The failure this defends against is quiet: a config says "off", the client
// omits a reasoning field, the server reasons anyway, and the run records
// reasoning tokens as completion tokens. Wall time, TTFT and the derived decode
// rate are all contaminated and nothing looks wrong.
//
// So the gate has two halves. The real configs must produce zero reasoning
// tokens *and* a request body containing the switch. Then a deliberately wrong
// control -- one that claims "off" and expresses it by omission -- must still
// reason. Without that second half, a mock that never reasoned would pass the
// first half forever.
func acceptNoThink(ctx context.Context, fixtureDir, layout, root string, timeScale float64) error {
	f, err := config.LoadFixture(fixtureDir)
	if err != nil {
		return err
	}
	stage, err := os.MkdirTemp("", "agentbench-nothink-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	body, err := mock.BuildResponse(ctx, f, mock.Reference, filepath.Join(stage, "canned"))
	if err != nil {
		return err
	}

	run := func(engineCfg, subdir string) (metrics.Record, string, error) {
		srv := &mock.Server{
			TimeScale: timeScale, ProfileFor: profileFromRequest,
			Respond: func(int) (string, string) { return body, "" },
		}
		ln, shutdown, err := srv.Listen("127.0.0.1:0")
		if err != nil {
			return metrics.Record{}, "", err
		}
		runDir, err := doRun(ctx, &runFlags{
			fixtureDir: fixtureDir, engines: engineCfg, layout: layout,
			contextPack: "base", thermal: "cold", runsDir: root,
			runID: "nothink-" + shortHash(subdir), runSubdir: subdir,
			endpoint: fmt.Sprintf("http://%s/v1", ln.Addr()), repeats: 1,
			caveats: []string{"Reasoning-control run against the in-repo mock; timings are not measurements."},
		})
		_ = shutdown(ctx)
		if err != nil {
			return metrics.Record{}, "", err
		}
		recs, err := metrics.ReadRecords(filepath.Join(runDir, "request.jsonl"))
		if err != nil {
			return metrics.Record{}, "", err
		}
		if len(recs) == 0 {
			return metrics.Record{}, "", fmt.Errorf("%s produced no records", subdir)
		}
		return recs[0], runDir, nil
	}

	// --- the real configs must actually be no-think ---
	rec, runDir, err := run("configs/engines/mock-ar.json,configs/engines/mock-dflash2.json", "nothink")
	if err != nil {
		return err
	}
	recs, err := metrics.ReadRecords(filepath.Join(runDir, "request.jsonl"))
	if err != nil {
		return err
	}
	for _, r := range recs {
		if r.ReasoningTokens == nil {
			return fmt.Errorf("%s reported no reasoning-token count at all, so "+
				"\"thinking: off\" cannot be checked", r.Engine)
		}
		if *r.ReasoningTokens != 0 {
			return fmt.Errorf("%s claims thinking is off but the server returned %d "+
				"reasoning tokens; the request did not actually disable reasoning",
				r.Engine, *r.ReasoningTokens)
		}
		sent, err := os.ReadFile(filepath.Join(runDir, r.Artifacts["request_body"]))
		if err != nil {
			return fmt.Errorf("%s: the request body was not retained: %w", r.Engine, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(sent, &doc); err != nil {
			return err
		}
		kw, _ := doc["chat_template_kwargs"].(map[string]any)
		if v, ok := kw["enable_thinking"].(bool); !ok || v {
			return fmt.Errorf("%s: the request it actually sent carries no no-think "+
				"switch: %s", r.Engine, truncate(string(sent), 200))
		}
		fmt.Printf("  %-26s 0 reasoning tokens, and the sent body carries enable_thinking=false\n",
			r.Engine)
	}
	_ = rec

	// --- and the check must not be vacuous ---
	ctl, _, err := run("configs/engines/controls/mock-nothink-omit-control.json", "nothink-control")
	if err != nil {
		return err
	}
	if ctl.ReasoningTokens == nil || *ctl.ReasoningTokens == 0 {
		return fmt.Errorf("the negative control produced no reasoning tokens. A config "+
			"that claims no-think by omission should still have reasoned, so the check "+
			"above proves nothing (got %v)", ctl.ReasoningTokens)
	}
	fmt.Printf("  %-26s %d reasoning tokens — the check above is not vacuous\n",
		ctl.Engine, *ctl.ReasoningTokens)
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:n]) + "..."
}
