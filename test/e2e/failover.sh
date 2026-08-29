#!/usr/bin/env bash
#
# End-to-end: start the stack, break a provider, and prove from the metrics that
# traffic moved and availability held.
#
# The assertions are on numbers that must have changed, not on the absence of an
# error. A run where the gateway silently served nothing at all would satisfy
# "no errors occurred"; it would not satisfy "bargain served at least N more
# requests than it had before apex broke".
#
# Every threshold below is deliberately loose. This is a timing-dependent test
# against a simulation with random latency, and a test that is precise about
# numbers it cannot control is a test that fails on a slow CI runner and teaches
# nobody anything. The thresholds check the direction and rough magnitude of a
# change; the exact values are reported so a human can read them.

set -uo pipefail

GATEWAY="${GATEWAY:-http://localhost:8080}"
PROM="${PROM:-http://localhost:9090}"

# How long to let each phase run. The breaker needs a few seconds to trip, and
# then the window needs enough requests in it to be worth measuring.
WARMUP="${WARMUP:-20}"
BREAK_WINDOW="${BREAK_WINDOW:-30}"
HEAL_WINDOW="${HEAL_WINDOW:-30}"

# Thresholds.
MAX_APEX_AFTER_BREAK=15   # in-flight requests may still land after the break
MIN_BARGAIN_AFTER_BREAK=60
MIN_APEX_AFTER_HEAL=40
MIN_AVAILABILITY=95       # percent, integer arithmetic throughout

COMPOSE="${COMPOSE:-docker compose}"
KEEP_STACK="${KEEP_STACK:-0}"

step() { printf '\n=== %s ===\n' "$1"; }
die() { printf '\ne2e: %s\n' "$1" >&2; exit 1; }

cleanup() {
	if [ "$KEEP_STACK" = "0" ]; then
		printf '\nstopping stack\n'
		$COMPOSE down --remove-orphans >/dev/null 2>&1 || true
	fi
}
trap cleanup EXIT

# counter NAME LABEL=VALUE... -- read one counter out of a saved metrics dump.
# Missing series read as 0, because a counter that has never been incremented is
# genuinely absent from a Prometheus exposition rather than present at zero.
counter() {
	local file="$1"; shift
	python3 - "$file" "$@" <<'PY'
import sys
path, name = sys.argv[1], sys.argv[2]
want = dict(kv.split("=", 1) for kv in sys.argv[3:])
total = 0.0
for line in open(path):
    line = line.strip()
    if not line or line.startswith("#") or not line.startswith(name):
        continue
    head, _, value = line.rpartition(" ")
    if "{" in head:
        series, labels = head.split("{", 1)
        labels = labels.rstrip("}")
    else:
        series, labels = head, ""
    if series != name:
        continue
    have = {}
    for part in labels.split('",'):
        if "=" in part:
            k, v = part.split("=", 1)
            have[k.strip()] = v.strip().strip('"')
    if all(have.get(k) == v for k, v in want.items()):
        total += float(value)
print(int(total))
PY
}

snapshot() {
	curl -sf "$GATEWAY/metrics" -o "$1" || die "could not scrape $GATEWAY/metrics"
}

# ── bring the stack up ────────────────────────────────────────────────────────
step 'starting stack'
$COMPOSE up --build -d || die 'docker compose up failed'

printf 'waiting for the gateway'
for _ in $(seq 1 60); do
	if curl -sf "$GATEWAY/healthz" >/dev/null 2>&1; then break; fi
	printf '.'; sleep 2
done
printf '\n'
curl -sf "$GATEWAY/healthz" >/dev/null || die "gateway never became healthy at $GATEWAY"

# Prometheus scraping proves the exporter and the scrape config agree, which is
# the part of the pipeline the gateway's own /metrics cannot vouch for.
printf 'waiting for prometheus to scrape the gateway'
scraped=0
for _ in $(seq 1 40); do
	up="$(curl -sf --get "$PROM/api/v1/query" \
		--data-urlencode 'query=up{job="switchyard"}' 2>/dev/null |
		python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["data"]["result"][0]["value"][1] if d["data"]["result"] else 0)' 2>/dev/null || echo 0)"
	if [ "$up" = "1" ]; then scraped=1; break; fi
	printf '.'; sleep 2
done
printf '\n'
[ "$scraped" = "1" ] || die "prometheus never scraped the gateway at $PROM"

curl -sf -X POST "$GATEWAY/admin/policy" -H 'content-type: application/json' \
	-d '{"policy":"failover"}' >/dev/null || die 'could not set the routing policy'

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"; cleanup' EXIT

# ── phase 1: steady state ─────────────────────────────────────────────────────
step "steady state for ${WARMUP}s"
sleep "$WARMUP"
snapshot "$tmp/a"

a_apex="$(counter "$tmp/a" switchyard_requests_total outcome=success provider=apex)"
a_bargain="$(counter "$tmp/a" switchyard_requests_total outcome=success provider=bargain)"
a_local="$(counter "$tmp/a" switchyard_requests_total outcome=success provider=local)"
a_success="$(counter "$tmp/a" switchyard_requests_total outcome=success)"
a_dropped=$(( $(counter "$tmp/a" switchyard_requests_total outcome=no_provider) +
              $(counter "$tmp/a" switchyard_requests_total outcome=stream_error) ))
a_failovers="$(counter "$tmp/a" switchyard_failovers_total)"

printf 'apex=%s bargain=%s local=%s dropped=%s failovers=%s\n' \
	"$a_apex" "$a_bargain" "$a_local" "$a_dropped" "$a_failovers"

[ "$a_apex" -gt 0 ] ||
	die "apex served $a_apex requests in steady state; there is no traffic to move"

# ── phase 2: break apex ───────────────────────────────────────────────────────
step 'breaking apex'
curl -sf -X POST "$GATEWAY/admin/inject" -H 'content-type: application/json' \
	-d '{"provider":"apex","mode":"error","rate":1}' >/dev/null ||
	die 'could not inject the fault'

printf 'observing for %ss\n' "$BREAK_WINDOW"
sleep "$BREAK_WINDOW"
snapshot "$tmp/b"

d_apex=$(( $(counter "$tmp/b" switchyard_requests_total outcome=success provider=apex) - a_apex ))
d_bargain=$(( $(counter "$tmp/b" switchyard_requests_total outcome=success provider=bargain) - a_bargain ))
d_local=$(( $(counter "$tmp/b" switchyard_requests_total outcome=success provider=local) - a_local ))
d_success=$(( $(counter "$tmp/b" switchyard_requests_total outcome=success) - a_success ))
b_dropped=$(( $(counter "$tmp/b" switchyard_requests_total outcome=no_provider) +
              $(counter "$tmp/b" switchyard_requests_total outcome=stream_error) ))
d_dropped=$(( b_dropped - a_dropped ))
d_failovers=$(( $(counter "$tmp/b" switchyard_failovers_total) - a_failovers ))

counted=$(( d_success + d_dropped ))
[ "$counted" -gt 0 ] || die 'no requests were counted during the break window'
availability=$(( d_success * 100 / counted ))

printf '\nduring the break window:\n'
printf '  apex served         %6d   (allowed at most %d)\n' "$d_apex" "$MAX_APEX_AFTER_BREAK"
printf '  bargain served      %6d   (needs at least %d)\n' "$d_bargain" "$MIN_BARGAIN_AFTER_BREAK"
printf '  local served        %6d\n' "$d_local"
printf '  failovers           %6d\n' "$d_failovers"
printf '  requests dropped    %6d of %d counted\n' "$d_dropped" "$counted"
printf '  availability        %6d%%  (needs at least %d%%)\n' "$availability" "$MIN_AVAILABILITY"

[ "$d_apex" -le "$MAX_APEX_AFTER_BREAK" ] ||
	die "apex served $d_apex requests after being broken, over the $MAX_APEX_AFTER_BREAK allowed for requests already in flight"
[ "$d_bargain" -ge "$MIN_BARGAIN_AFTER_BREAK" ] ||
	die "bargain served $d_bargain requests, under the $MIN_BARGAIN_AFTER_BREAK expected; traffic did not move"
[ "$d_failovers" -gt 0 ] ||
	die "the failover counter did not move; the router never recorded a reroute"
[ "$availability" -ge "$MIN_AVAILABILITY" ] ||
	die "availability was ${availability}%, under the ${MIN_AVAILABILITY}% floor; failover did not hold the SLO"

state="$(curl -sf "$GATEWAY/admin/state" |
	python3 -c 'import json,sys; print([p["breaker"]["state"] for p in json.load(sys.stdin)["providers"] if p["name"]=="apex"][0])')"
printf '  apex breaker        %6s\n' "$state"
[ "$state" = "open" ] || [ "$state" = "recovering" ] ||
	die "apex breaker is '$state' while apex is broken; expected open or recovering"

# ── phase 3: heal apex ────────────────────────────────────────────────────────
step 'healing apex'
curl -sf -X POST "$GATEWAY/admin/inject" -H 'content-type: application/json' \
	-d '{"provider":"apex","mode":"healthy"}' >/dev/null ||
	die 'could not clear the fault'

printf 'observing for %ss\n' "$HEAL_WINDOW"
sleep "$HEAL_WINDOW"
snapshot "$tmp/c"

h_apex=$(( $(counter "$tmp/c" switchyard_requests_total outcome=success provider=apex) -
           $(counter "$tmp/b" switchyard_requests_total outcome=success provider=apex) ))
h_success=$(( $(counter "$tmp/c" switchyard_requests_total outcome=success) - d_success - a_success ))
c_dropped=$(( $(counter "$tmp/c" switchyard_requests_total outcome=no_provider) +
              $(counter "$tmp/c" switchyard_requests_total outcome=stream_error) ))
h_dropped=$(( c_dropped - b_dropped ))

h_counted=$(( h_success + h_dropped ))
[ "$h_counted" -gt 0 ] || die 'no requests were counted during the heal window'
h_availability=$(( h_success * 100 / h_counted ))

state="$(curl -sf "$GATEWAY/admin/state" |
	python3 -c 'import json,sys; print([p["breaker"]["state"] for p in json.load(sys.stdin)["providers"] if p["name"]=="apex"][0])')"

printf '\nduring the heal window:\n'
printf '  apex served         %6d   (needs at least %d)\n' "$h_apex" "$MIN_APEX_AFTER_HEAL"
printf '  requests dropped    %6d of %d counted\n' "$h_dropped" "$h_counted"
printf '  availability        %6d%%  (needs at least %d%%)\n' "$h_availability" "$MIN_AVAILABILITY"
printf '  apex breaker        %6s\n' "$state"

[ "$h_apex" -ge "$MIN_APEX_AFTER_HEAL" ] ||
	die "apex served $h_apex requests after healing, under the $MIN_APEX_AFTER_HEAL expected; traffic did not return"
[ "$h_availability" -ge "$MIN_AVAILABILITY" ] ||
	die "availability during recovery was ${h_availability}%, under the ${MIN_AVAILABILITY}% floor; the return caused a second outage"
[ "$state" = "closed" ] ||
	die "apex breaker is '$state' ${HEAL_WINDOW}s after healing; expected closed"

# ── the same numbers, through Prometheus ──────────────────────────────────────
# Reading them back out of Prometheus proves the exporter, the scrape config and
# the queries the dashboards use all line up, rather than only the gateway's own
# view of itself.
step 'through prometheus'
prom_providers="$(curl -sf --get "$PROM/api/v1/query" \
	--data-urlencode 'query=count(count by (provider) (switchyard_requests_total{outcome="success"}))' |
	python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["data"]["result"][0]["value"][1] if d["data"]["result"] else 0)')"
printf '  providers with successful requests: %s\n' "$prom_providers"
[ "$prom_providers" -ge 2 ] ||
	die "prometheus sees $prom_providers provider(s) with traffic; the failover is not visible in the scraped data"

step 'totals'
printf '  steady state:  apex %d, bargain %d, local %d\n' "$a_apex" "$a_bargain" "$a_local"
printf '  apex broken:   apex +%d, bargain +%d, local +%d, dropped %d, availability %d%%\n' \
	"$d_apex" "$d_bargain" "$d_local" "$d_dropped" "$availability"
printf '  apex healed:   apex +%d, dropped %d, availability %d%%\n' \
	"$h_apex" "$h_dropped" "$h_availability"
