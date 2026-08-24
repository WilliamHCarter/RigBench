package metrics

import (
	"encoding/json"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// RunIdentity is everything needed to decide whether two runs are comparable.
//
// Freezing a benchmark release means freezing hashes, not filenames, so every
// content-addressable input appears here as a digest. Fields the host cannot
// answer are left empty rather than guessed; an empty GPU string means "not
// collected", which is why the collector records how it tried.
type RunIdentity struct {
	Schema    string    `json:"schema"`
	RunID     string    `json:"run_id"`
	StartedAt time.Time `json:"started_at"`

	// Benchmark identity.
	BenchmarkGitSHA   string `json:"benchmark_git_sha"`
	BenchmarkGitDirty bool   `json:"benchmark_git_dirty"`

	// Fixture identity: the manifest digest covers every byte the prompt and
	// the gates are built from.
	FixtureID       string            `json:"fixture_id"`
	FixtureVersion  string            `json:"fixture_version"`
	FixtureManifest string            `json:"fixture_manifest_sha256"`
	FixtureFiles    map[string]string `json:"fixture_files_sha256,omitempty"`

	// Engine identity. Everything here is nullable in spirit: an empty string
	// means the engine did not tell us.
	Engines []EngineIdentity `json:"engines"`

	// Host identity.
	Host      HostIdentity `json:"host"`
	Toolchain Toolchain    `json:"toolchain"`

	// Thermal describes how the run was staged, because "warm" is never
	// inferred from elapsed time.
	Thermal      string `json:"thermal"`
	WarmupPolicy string `json:"warmup_policy"`
}

type EngineIdentity struct {
	Name             string            `json:"name"`
	Endpoint         string            `json:"endpoint"`
	Model            string            `json:"model"`
	EngineCommit     string            `json:"engine_commit,omitempty"`
	ModelHash        string            `json:"model_hash,omitempty"`
	DraftHash        string            `json:"draft_hash,omitempty"`
	TargetQuant      string            `json:"target_quant,omitempty"`
	KVMode           string            `json:"kv_mode,omitempty"`
	SpeculationMode  string            `json:"speculation_mode,omitempty"`
	NonDefaultKnobs  map[string]string `json:"non_default_knobs,omitempty"`
	TelemetryAdapter string            `json:"telemetry_adapter"`

	// Attested records whether this run *produced* the engine state it claims,
	// or merely asserted it. An OpenAI-compatible request cannot select a
	// speculation mode or a KV quantization: those are process-level settings,
	// so two configs sent to one already-running daemon yield two differently
	// labelled rows from one actual configuration. Attested is false for such a
	// run, and the summary says so rather than presenting the labels as facts.
	Attested          bool   `json:"attested"`
	AttestationMethod string `json:"attestation_method"`
	PreparationLog    string `json:"preparation_log,omitempty"`
	ProbeArtifact     string `json:"probe_artifact,omitempty"`
}

type HostIdentity struct {
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	Kernel      string `json:"kernel,omitempty"`
	CPUs        int    `json:"cpus"`
	GPU         string `json:"gpu,omitempty"`
	GPUDriver   string `json:"gpu_driver,omitempty"`
	ROCmVersion string `json:"rocm_version,omitempty"`
	// Collected records which probes were attempted, so an empty GPU field is
	// distinguishable from a field nobody looked for.
	Collected []string `json:"collected"`
}

type Toolchain struct {
	Go  string `json:"go"`
	Zig string `json:"zig,omitempty"`
	// ZigPin is the version the fixture's build.zig.zon pins. Under anyzig this
	// is the single source of truth for which compiler ran.
	ZigPin string `json:"zig_pin,omitempty"`
}

// CollectHost probes the host for identity fields. It never fails: an
// unavailable probe leaves its field empty and is still listed in Collected.
func CollectHost() HostIdentity {
	h := HostIdentity{
		OS:   runtime.GOOS,
		Arch: runtime.GOARCH,
		CPUs: runtime.NumCPU(),
	}
	try := func(name string, args ...string) string {
		h.Collected = append(h.Collected, name)
		out, err := exec.Command(name, args...).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	switch runtime.GOOS {
	case "darwin":
		h.Kernel = try("uname", "-sr")
		h.GPU = firstLine(try("sh", "-c",
			"system_profiler SPDisplaysDataType 2>/dev/null | awk -F': ' '/Chipset Model/{print $2; exit}'"))
	case "linux":
		h.Kernel = try("uname", "-sr")
		h.GPU = firstLine(try("sh", "-c",
			"rocm-smi --showproductname 2>/dev/null | head -20"))
		h.ROCmVersion = firstLine(try("sh", "-c",
			"cat /opt/rocm/.info/version 2>/dev/null"))
		h.GPUDriver = firstLine(try("sh", "-c",
			"modinfo amdgpu 2>/dev/null | awk '/^version:/{print $2}'"))
	}
	return h
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// GitDescribe returns the HEAD SHA of the repository containing dir and whether
// the working tree is dirty. A dirty tree is recorded, never cleaned.
func GitDescribe(dir string) (sha string, dirty bool) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err == nil {
		sha = strings.TrimSpace(string(out))
	}
	st, err := exec.Command("git", "-C", dir, "status", "--porcelain").Output()
	if err == nil {
		dirty = len(strings.TrimSpace(string(st))) > 0
	}
	return sha, dirty
}

func (r *RunIdentity) Save(path string) error {
	r.Schema = "agentbench/run/v1"
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
