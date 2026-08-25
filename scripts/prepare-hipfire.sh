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
HEALTH_MODEL="${AGENTBENCH_HEALTH_MODEL:-$MODEL}"
HOST="${AGENTBENCH_HOST:-127.0.0.1}"
PORT="${AGENTBENCH_PORT:-11435}"
# A knob-supplied draft path wins over the default: that is how a drafter
# sweep changes artifacts without editing this script.
DRAFT_F16="${AGENTBENCH_KNOB_DRAFT_FILE:-${AGENTBENCH_DRAFT_F16:-$HOME/.hipfire/models/qwen3.8-27b-dflash2-f16.hfq}}"
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
      if [[ -n "$model" && "$model" == "$HEALTH_MODEL" && -z "$loading" ]]; then
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
  log "FATAL: ${HEALTH_MODEL} was not resident after ${READY_TIMEOUT}s"
  return 1
}

# apply_knobs applies whatever the engine config declared.
#
# The harness hands every set tuning axis to this hook as AGENTBENCH_KNOB_*.
# It has no opinion about the values -- that is deliberate, so a sweep can be
# driven from outside without editing RigBench. This function is where an
# opinion-free axis meets an engine-specific command, and it is the only place
# that needs to know how Hipfire spells things.
#
# An unset knob is ABSENT from the environment, not empty, so "leave it alone"
# is distinguishable from "set it to nothing".
apply_knobs() {
  set_if() {  # set_if <env-var> <hipfire-key>
    local val="${!1:-}"
    [[ -n "$val" ]] || return 0
    log "knob $2=$val"
    hipfire config "$MODEL" set "$2" "$val"
  }

  set_if AGENTBENCH_KNOB_KV_DTYPE            kv_cache
  set_if AGENTBENCH_KNOB_SPECULATION_METHOD  speculation
  set_if AGENTBENCH_KNOB_SPECULATIVE_BLOCK   dflash_block
  set_if AGENTBENCH_KNOB_SPECULATIVE_BUDGET  dflash_budget
  set_if AGENTBENCH_KNOB_ADAPTIVE_BLOCK      dflash_adaptive
  set_if AGENTBENCH_KNOB_VERIFY_MODE         verify_mode
  set_if AGENTBENCH_KNOB_PM4_VERIFY          verify_pm4
  set_if AGENTBENCH_KNOB_PREFILL_SPECULATION prefill_compression
  set_if AGENTBENCH_KNOB_PROMPT_CACHE_MODE   prompt_cache
  set_if AGENTBENCH_KNOB_PROMPT_CACHE_CAPACITY prompt_cache_capacity
  set_if AGENTBENCH_KNOB_CONTEXT_TOKENS      context_length

  # Anything the harness passed that this function does not map is logged and
  # NOT applied. Silently dropping a requested knob would produce a row labelled
  # with a setting the server never had.
  local unmapped=0
  while IFS='=' read -r k _; do
    case "$k" in
      AGENTBENCH_KNOB_KV_DTYPE|AGENTBENCH_KNOB_SPECULATION_METHOD|\
      AGENTBENCH_KNOB_SPECULATIVE_BLOCK|AGENTBENCH_KNOB_SPECULATIVE_BUDGET|\
      AGENTBENCH_KNOB_ADAPTIVE_BLOCK|AGENTBENCH_KNOB_VERIFY_MODE|\
      AGENTBENCH_KNOB_PM4_VERIFY|AGENTBENCH_KNOB_PREFILL_SPECULATION|\
      AGENTBENCH_KNOB_PROMPT_CACHE_MODE|AGENTBENCH_KNOB_PROMPT_CACHE_CAPACITY|\
      AGENTBENCH_KNOB_CONTEXT_TOKENS|AGENTBENCH_KNOB_TARGET_*|AGENTBENCH_KNOB_DRAFT_*|\
      AGENTBENCH_KNOB_GPU|AGENTBENCH_KNOB_REPLICA_ID|AGENTBENCH_KNOB_MAX_OUTPUT_TOKENS|\
      AGENTBENCH_KNOB_CONCURRENCY) ;;
      AGENTBENCH_KNOB_*)
        log "WARNING: unmapped knob $k -- requested but NOT applied"
        unmapped=$(( unmapped + 1 ))
        ;;
    esac
  done < <(env | grep '^AGENTBENCH_KNOB_' || true)
  if (( unmapped > 0 )); then
    log "FATAL: ${unmapped} requested knob(s) have no mapping here."
    log "Add them above rather than recording a row labelled with a setting the server never had."
    return 1
  fi
}

common_config() {
  apply_knobs
}

log "engine=${ENGINE} model=${MODEL} health_model=${HEALTH_MODEL} endpoint=${HOST}:${PORT}"

case "$ENGINE" in
  ar)
    hipfire stop || true
    hipfire config "$MODEL" set dflash_mode off
    common_config
    hipfire serve "$MODEL" "${HOST}:${PORT}" --idle-timeout 0 -d
    ;;

  dflash2-f16)
    [[ -r "$DRAFT_F16" ]] || { log "FATAL: drafter not readable at $DRAFT_F16"; exit 2; }
    hipfire stop || true
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
hipfire config "$MODEL" list || log "(hipfire config list unavailable)"
