#!/usr/bin/env bash
# Sourced by isolated drills. No side effects until explicitly invoked.
DRILL_CONTAINERS=()
DRILL_NETWORKS=()
declare -A DRILL_GROUP=()

drill_label() {
  local kind="$1" id="$2" field='.Labels'
  [[ "$kind" != container ]] || field='.Config.Labels'
  docker "$kind" inspect --format "{{index $field \"io.epocha.drill\"}}" -- "$id" 2>/dev/null
}
drill_register() {
  local kind="$1" id="$2" group="$3" owner
  [[ "$id" =~ ^[a-f0-9]{64}$ && -n "${DRILL_OWNER:-}" ]] || return 1
  owner="$(drill_label "$kind" "$id")" || return 1
  [[ "$owner" == "$DRILL_OWNER" ]] || return 1
  [[ -z "${DRILL_GROUP[$kind:$id]:-}" ]] || return 0
  DRILL_GROUP[$kind:$id]="$group"
  case "$kind" in
    container) DRILL_CONTAINERS+=("$id") ;;
    network) DRILL_NETWORKS+=("$id") ;;
    *) return 1 ;;
  esac
}
drill_collect() {
  local project="$1" ids id
  ids="$(docker ps -aq --no-trunc --filter "label=com.docker.compose.project=$project")" || return 1
  while IFS= read -r id; do [[ -z "$id" ]] || drill_register container "$id" "$project" || return 1; done <<<"$ids"
  ids="$(docker network ls -q --no-trunc --filter "label=com.docker.compose.project=$project")" || return 1
  while IFS= read -r id; do [[ -z "$id" ]] || drill_register network "$id" "$project" || return 1; done <<<"$ids"
  # These recipes have bind mounts only. An unexpected named volume is foreign.
  ids="$(docker volume ls -q --filter "label=com.docker.compose.project=$project")" || return 1
  [[ -z "$ids" ]]
}
cleanup_owned_resources() {
  local group="${1:-}" id owner
  for id in "${DRILL_CONTAINERS[@]}"; do
    [[ -z "$group" || "${DRILL_GROUP[container:$id]}" == "$group" ]] || continue
    owner="$(drill_label container "$id")" || continue
    [[ "$owner" == "$DRILL_OWNER" ]] || continue
    docker rm -f -- "$id" >/dev/null 2>&1 || true
  done
  for id in "${DRILL_NETWORKS[@]}"; do
    [[ -z "$group" || "${DRILL_GROUP[network:$id]}" == "$group" ]] || continue
    owner="$(drill_label network "$id")" || continue
    [[ "$owner" == "$DRILL_OWNER" ]] || continue
    docker network rm -- "$id" >/dev/null 2>&1 || true
  done
}

drill_remove_container() {
  local id="$1" owner
  [[ -n "${DRILL_GROUP[container:$id]:-}" ]] || return 1
  owner="$(drill_label container "$id")" || return 0 # already removed
  [[ "$owner" == "$DRILL_OWNER" ]] || return 1
  docker rm -f -- "$id" >/dev/null
}

drill_ownership_selftest() (
  set -euo pipefail
  local scenario="$1" dir record cid nid foreign relabeled=0
  dir="$(mktemp -d)"; record="$dir/docker.log"
  trap 'rm -rf -- "$dir"' EXIT
  : >"$record"
  DRILL_OWNER=fixture-owner
  cid="$(printf '%064d' 1)"; nid="$(printf '%064d' 2)"; foreign="$(printf '%064d' 3)"
  docker() {
    if [[ "$2" == inspect ]]; then
      [[ "$relabeled" == 0 ]] || { echo foreign-owner; return; }
      [[ "${*: -1}" != "$foreign" ]] || { echo foreign-owner; return; }
      echo "$DRILL_OWNER"; return
    fi
    printf '%s\n' "$*" >>"$record"
  }
  case "$scenario" in
    preflight) : ;;
    created|replaced-name)
      drill_register container "$cid" fixture
      drill_register network "$nid" fixture
      # A replacement using the old human name has a different ID and is never selected.
      ;;
    foreign-label)
      if drill_register container "$foreign" fixture; then exit 1; fi
      ;;
    relabeled)
      drill_register container "$cid" fixture
      drill_register network "$nid" fixture
      relabeled=1
      if drill_remove_container "$cid"; then exit 1; fi
      ;;
    *) exit 64 ;;
  esac
  cleanup_owned_resources
  case "$scenario" in
    created|replaced-name)
      [[ "$(cat "$record")" == "$(printf 'rm -f -- %s\nnetwork rm -- %s' "$cid" "$nid")" ]] ;;
    *) [[ ! -s "$record" ]] ;;
  esac
  printf '[stage5b] ownership selftest PASS: %s\n' "$scenario"
)
