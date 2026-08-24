#!/usr/bin/env bash
#
# AgentBench-01 engine preparation hook.
#
#   usage: prepare-hipfire.sh <engine-name>
#
# Invoked by `agentbench run -before-engine ./scripts/prepare-hipfire.sh` once
# per engine config, before anything is measured. It owns *all* Hipfire state:
# stopping the daemon, setting speculation and KV mode, loading the right
# drafter, restarting, and waiting until the model is actually resident.
#
# Why this exists: an OpenAI-compatible request carries model, messages and
# sampling. It cannot select a speculation mode or a KV quantization -- on
# Hipfire those are process-level settings. Without this hook, running an AR
# config and a DFlash config against one already-running daemon produces two
# differently *labelled* rows from one actual configuration, which is worse
# than no result because it looks like one.
#
# ---------------------------------------------------------------------------
# NOT VERIFIED FROM THIS WORKSTATION.
#
# The hipfire invocations below are transcribed from the review that requested
# this hook. No Hipfire server is reachable from the machine this was written
# on, so the flag names, the config keys and the /health schema are unconfirmed.
# Run it once by hand before trusting a campaign, and correct anything that
# disagrees with your build. The readiness gate and the failure behaviour are
# the parts that must not be relaxed: exiting non-zero here aborts the run,
# which is the whole point.
# ---------------------------------------------------------------------------

set -euo pipefail

ENGINE="${1:?usage: prepare-hipfire.sh <engine-name>}"

MODEL="${AGENTBENCH_MODEL:-qwen3.8:27b-fast}"
HOST="${AGENTBENCH_HOST:-127.0.0.1}"
PORT="${AGENTBENCH_PORT:-11435}"
DRAFT_F16="${AGENTBENCH_DRAFT_F16:-$HOME/.hipfire/models/qwen3.8-27b-dflash2-f16.hfq}"
READY_TIMEOUT="${AGENTBENCH_READY_TIMEOUT:-300}"

log() { printf '[prepare-hipfire] %s\n' "$*"; }

need() {
  command -v "$1" >/dev/null 2>&1 || { log "FATAL: $1 is not on PATH"; exit 2; }
}

# The in-repo mock needs no server preparation and no Hipfire toolchain. Handled
# before the tool checks so the acceptance gate runs on a workstation with no
# rig, and named explicitly rather than caught by a permissive default: an
# unrecognised engine must be an error, or a typo in -engines would silently
# skip preparation and mislabel a row.
case "$ENGINE" in
  mock-ar|mock-dflash2)
    log "engine=${ENGINE}: in-repo mock, no preparation needed"
    exit 0
    ;;
esac

need hipfire
need curl
need jq

# wait_ready blocks until /health reports the target model resident.
#
# Polling /health rather than sleeping is deliberate: a fixed sleep either wastes
# time or, worse, lets the first measured request land mid-load and be recorded
# as a cold number that is really a loading number.
wait_ready() {
  local deadline=$(( SECONDS + READY_TIMEOUT ))
  local body loading model
  while (( SECONDS < deadline )); do
    if body="$(curl -fsS --max-time 5 "http://${HOST}:${PORT}/health" 2>/dev/null)"; then
      model="$(printf '%s' "$body" | jq -r '.model // empty')"
      loading="$(printf '%s' "$body" | jq -r '.loading_model // empty')"
      if [[ -n "$model" && "$model" == "$MODEL" && -z "$loading" ]]; then
        log "ready: model=$model"
        printf '%s\n' "$body" | jq .
        return 0
      fi
      log "waiting: model=${model:-none} loading=${loading:-none}"
    else
      log "waiting: /health not answering yet"
    fi
    sleep 2
  done
  log "FATAL: ${MODEL} was not resident after ${READY_TIMEOUT}s"
  return 1
}

common_config() {
  hipfire config "$MODEL" set kv_cache q8
  hipfire config "$MODEL" set prefill_compression off
}

log "engine=${ENGINE} model=${MODEL} endpoint=${HOST}:${PORT}"

case "$ENGINE" in
  ar)
    hipfire stop || true
    hipfire config "$MODEL" set speculation off
    hipfire config "$MODEL" set dflash_mode off
    common_config
    hipfire serve "$MODEL" "${HOST}:${PORT}" --idle-timeout 0 -d
    ;;

  dflash2-f16)
    [[ -r "$DRAFT_F16" ]] || { log "FATAL: drafter not readable at $DRAFT_F16"; exit 2; }
    hipfire stop || true
    hipfire config "$MODEL" set speculation dflash
    hipfire config "$MODEL" set dflash_mode on
    common_config
    HIPFIRE_DFLASH_DRAFT="$DRAFT_F16" \
      hipfire serve "$MODEL" "${HOST}:${PORT}" --idle-timeout 0 -d
    ;;

  *)
    log "FATAL: no preparation recipe for engine '${ENGINE}'."
    log "Add one here rather than letting the run proceed against an unknown state."
    exit 2
    ;;
esac

wait_ready

# Evidence. The run keeps this output, so a row labelled 'ar' can be checked
# against what the daemon actually reported at the moment it was prepared.
log "resulting configuration:"
hipfire config "$MODEL" show || log "(hipfire config show unavailable)"
