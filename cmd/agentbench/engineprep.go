package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/WilliamHCarter/RigBench/internal/config"
	"github.com/WilliamHCarter/RigBench/internal/executor"
	"github.com/WilliamHCarter/RigBench/internal/metrics"
)

// Engine identity is a claim until something makes it true and something else
// checks it.
//
// An engine config's `speculation_mode`, `kv_mode` and `target_quant` describe a
// server configuration that the OpenAI-compatible request path cannot select:
// on Hipfire and most local engines those are process-level settings. So a run
// that sends two configs to one already-running daemon produces two differently
// *labelled* rows from one actual configuration. That is worse than no result,
// because it looks like a result.
//
// Two mechanisms close it, and both are recorded:
//
//   - a preparation hook, invoked as `<script> <engine-name>` before each
//     engine's turns, which owns all server lifecycle and readiness;
//   - an identity probe, a GET whose response is stored and optionally asserted
//     against, so the label is checked and not merely produced.
//
// A run with neither is not refused -- the mock lane legitimately has neither --
// but every such row is marked unattested and the summary says so.

// engineAttestation is what a run can show for one engine's identity.
type engineAttestation struct {
	Prepared      bool
	PreparedLog   string
	Probed        bool
	ProbeArtifact string
	// Checked is true only when the probe asserted something and the assertion
	// held. A probe with an empty `require` block records the server's answer
	// and verifies nothing, and must not be described as verification.
	Checked  bool
	Recorded map[string]string
	Method   string
	// KnobEnv is exactly what was handed to the hook, kept so a run's request
	// can be compared against what the server reports it resolved to.
	KnobEnv []string
}

// prepareEngine runs the preparation hook and the identity probe for one engine.
func prepareEngine(ctx context.Context, e *config.Engine, hook, endpoint, runDir, suffix string,
	timeout time.Duration) (*engineAttestation, error) {

	att := &engineAttestation{Recorded: map[string]string{}}
	dir := filepath.Join(runDir, "artifacts", "engine-prep")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	if hook != "" {
		abs, err := filepath.Abs(hook)
		if err != nil {
			return nil, err
		}
		// Every set tuning axis reaches the hook as AGENTBENCH_KNOB_*. The
		// harness declares the axis and records what was asked for; the hook
		// knows how to apply it. Neither needs an opinion about the value.
		env := e.Knobs.Env()
		// The hook is given the preparation profile, not the display name: the
		// two differ whenever a reasoning variant reuses a server setup.
		r := executor.RunEnv(ctx, ".", []string{abs, e.PrepareProfile}, env, timeout)
		logPath := filepath.Join(dir, e.Name+suffix+".prepare.log")
		body := fmt.Sprintf("$ %s %s\n(knobs: %s)\nexit %d  duration %s  timed_out=%v unavailable=%v\n\n%s\n",
			abs, e.PrepareProfile, strings.Join(env, " "),
			r.ExitCode, r.Duration.Round(time.Millisecond),
			r.TimedOut, r.Unavailable, r.Combined())
		_ = os.WriteFile(logPath, []byte(body), 0o644)
		att.PreparedLog = filepath.ToSlash(filepath.Join("artifacts", "engine-prep", e.Name+suffix+".prepare.log"))
		if !r.OK() {
			return nil, fmt.Errorf("preparation hook failed for %s (profile %q, exit %d): %s\nsee %s",
				e.Name, e.PrepareProfile, r.ExitCode, firstLineOf(r.Combined()), logPath)
		}
		att.Prepared = true
		att.KnobEnv = env
	}

	if e.IdentityProbe != nil {
		raw, err := probe(ctx, endpoint, e.IdentityProbe.Path, timeout)
		if err != nil {
			return nil, fmt.Errorf("identity probe for %s: %w", e.Name, err)
		}
		probePath := filepath.Join(dir, e.Name+suffix+".probe.json")
		_ = os.WriteFile(probePath, raw, 0o644)
		att.ProbeArtifact = filepath.ToSlash(filepath.Join("artifacts", "engine-prep", e.Name+suffix+".probe.json"))

		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("identity probe for %s returned non-JSON: %w", e.Name, err)
		}
		var mismatches []string
		for path, want := range e.IdentityProbe.Require {
			got, ok := lookup(doc, path)
			switch {
			case !ok:
				mismatches = append(mismatches, fmt.Sprintf("%s is absent, want %q", path, want))
			case got != want:
				mismatches = append(mismatches, fmt.Sprintf("%s is %q, want %q", path, got, want))
			}
		}
		if len(mismatches) > 0 {
			sort.Strings(mismatches)
			return nil, fmt.Errorf("the server does not match what engine config %q claims: %s\n"+
				"refusing to record rows labelled %q against a server in a different state (see %s)",
				e.Name, strings.Join(mismatches, "; "), e.Name, probePath)
		}
		for path, field := range e.IdentityProbe.Record {
			if got, ok := lookup(doc, path); ok {
				att.Recorded[field] = got
			}
		}
		att.Probed = true
		att.Checked = len(e.IdentityProbe.Require) > 0
	}

	switch {
	case att.Prepared && att.Checked:
		att.Method = "prepared by hook and verified by probe"
	case att.Prepared && att.Probed:
		att.Method = "prepared by hook; the probe recorded the server's answer but " +
			"asserted nothing about it (identity_probe.require is empty)"
	case att.Prepared:
		att.Method = "prepared by hook; not probed"
	case att.Checked:
		att.Method = "verified by probe; not prepared by this run"
	case att.Probed:
		att.Method = "probed but not verified (identity_probe.require is empty), " +
			"and not prepared by this run"
	default:
		att.Method = "unattested: the config's engine state was asserted, not produced or checked"
	}
	return att, nil
}

// probe GETs path against the endpoint's scheme and host. The path is resolved
// against the host and not against the endpoint path, because a health route
// usually sits outside /v1.
func probe(ctx context.Context, endpoint, path string, timeout time.Duration) ([]byte, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	target := (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: path}).String()

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", target, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: http %d: %s", target, resp.StatusCode,
			strings.TrimSpace(string(body)))
	}
	return body, nil
}

// lookup resolves a dotted path in decoded JSON and renders the value as a
// string. Only scalars are addressable; a path landing on an object or array is
// reported as absent rather than stringified into something nobody can predict.
func lookup(doc any, path string) (string, bool) {
	cur := doc
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		cur, ok = m[seg]
		if !ok {
			return "", false
		}
	}
	switch v := cur.(type) {
	case string:
		return v, true
	case bool:
		return strconv.FormatBool(v), true
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10), true
		}
		return strconv.FormatFloat(v, 'g', -1, 64), true
	case nil:
		return "", false
	}
	return "", false
}

// applyAttestation folds a probe's recorded values into the run's engine
// identity, so reproducibility fields stop depending on someone remembering to
// paste a hash into a config file.
func applyAttestation(id *metrics.EngineIdentity, att *engineAttestation) {
	// Attested means this run produced or checked the state, not merely that it
	// asked. A probe that asserts nothing is a recording, not an attestation.
	id.Attested = att.Prepared || att.Checked
	id.AttestationMethod = att.Method
	id.PreparationLog = att.PreparedLog
	id.ProbeArtifact = att.ProbeArtifact
	for field, val := range att.Recorded {
		switch field {
		case "engine_commit":
			id.EngineCommit = val
		case "model_hash":
			id.ModelHash = val
		case "draft_hash":
			id.DraftHash = val
		case "model", "resolved_model":
			id.ResolvedModel = val
		default:
			if id.NonDefaultKnobs == nil {
				id.NonDefaultKnobs = map[string]string{}
			}
			id.NonDefaultKnobs[field] = val
		}
	}
}

func firstLineOf(s string) string {
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// hashArtifacts digests the model files an engine config names, when they are
// readable from this host.
//
// A hash the harness computes beats one a human pastes into a config: the paste
// is a snapshot of what was true when somebody last looked. When the file is not
// readable -- the usual case for a remote rig -- the config's declared value
// stands and is recorded as declared rather than verified.
func hashArtifacts(id *metrics.EngineIdentity, e *config.Engine) {
	digest := func(path string) string {
		if path == "" {
			return ""
		}
		f, err := os.Open(path)
		if err != nil {
			return ""
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return ""
		}
		return hex.EncodeToString(h.Sum(nil))
	}
	if got := digest(e.Knobs.TargetFile); got != "" {
		id.TargetSHA256 = got
	}
	if got := digest(e.Knobs.DraftFile); got != "" {
		id.DraftSHA256 = got
	}
	// A resolved model reported by the probe never overwrites the request.
	if id.ResolvedModel == "" {
		if v, ok := att_recorded(id); ok {
			id.ResolvedModel = v
		}
	}
}

// att_recorded pulls a resolved model out of whatever the probe recorded into
// the identity, without inventing one.
func att_recorded(id *metrics.EngineIdentity) (string, bool) {
	if id.NonDefaultKnobs == nil {
		return "", false
	}
	v, ok := id.NonDefaultKnobs["resolved_model"]
	return v, ok
}
