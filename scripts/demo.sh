#!/usr/bin/env bash
#
# The demo loop, as a person would drive it by hand:
#
#   ask -> see who answered -> break exactly that provider -> ask again
#   -> see someone else answer, with the same prompt and no client change.
#
# The dashboard shows failover in aggregate, which is the right way to watch a
# fleet and the wrong way to understand a single request. This walks one
# request at a time so the routing decision is legible without Grafana.
#
# It breaks whichever provider actually answered rather than assuming apex.
# Hardcoding the victim would make the script lie the first time the policy or
# the priority order changes.

set -uo pipefail

GATEWAY="${GATEWAY:-http://localhost:8080}"
PROMPT="${PROMPT:-Why does a gateway need to know the difference between a 429 and a 503?}"

bold=$'\033[1m'; dim=$'\033[2m'; green=$'\033[32m'; yellow=$'\033[33m'; reset=$'\033[0m'
[ -t 1 ] || { bold=''; dim=''; green=''; yellow=''; reset=''; }

say()  { printf '%s\n' "$*"; }
step() { printf '\n%s%s%s\n' "$bold" "$*" "$reset"; }
cmd()  { printf '%s  $ %s%s\n' "$dim" "$*" "$reset"; }

fail() { printf '\ndemo: %s\n' "$1" >&2; exit 1; }

# One request. Prints "provider policy failovers" from the response headers.
#
# -D - writes the headers to stdout and -o /dev/null discards the streamed
# body: the routing decision is in the headers, and the tokens are not the
# point here. max_tokens is small so a step takes a moment, not a minute.
ask() {
	local hdrs
	hdrs="$(curl --silent --show-error --fail-with-body \
		-D - -o /dev/null \
		-X POST "$GATEWAY/v1/chat" \
		-H 'content-type: application/json' \
		-d "{\"prompt\":$(printf '%s' "$PROMPT" | sed 's/"/\\"/g; s/^/"/; s/$/"/'),\"max_tokens\":24}" \
		2>&1)" || return 1
	printf '%s' "$hdrs" | tr -d '\r' | awk '
		BEGIN { IGNORECASE = 1 }
		tolower($1) == "x-switchyard-provider:"  { p = $2 }
		tolower($1) == "x-switchyard-policy:"    { c = $2 }
		tolower($1) == "x-switchyard-failovers:" { f = $2 }
		END { print p, c, f }'
}

inject() { # provider mode
	curl --silent --show-error --fail-with-body -X POST "$GATEWAY/admin/inject" \
		-H 'content-type: application/json' \
		-d "{\"provider\":\"$1\",\"mode\":\"$2\",\"rate\":1}" >/dev/null
}

# --- preflight ----------------------------------------------------------------
curl --silent --fail --max-time 3 "$GATEWAY/healthz" >/dev/null 2>&1 ||
	fail "no gateway at $GATEWAY. Start it with 'make up' (or 'go run ./cmd/switchyard'), then rerun."

say "${bold}Switchyard demo loop${reset}  --  gateway at $GATEWAY"
say "${dim}Same prompt every time. The only thing that changes is who can serve it.${reset}"

# --- 1. a known-good starting point -------------------------------------------
step '1. Reset every provider to healthy, policy back to primary-first.'
cmd "make reset"
for p in apex bargain local; do inject "$p" healthy; done
curl --silent --show-error -X POST "$GATEWAY/admin/policy" \
	-H 'content-type: application/json' -d '{"policy":"failover"}' >/dev/null
say "   all three providers healthy."

# --- 2. ask ------------------------------------------------------------------
step '2. Send one request. The response headers name who served it.'
cmd "curl -sS -D - -o /dev/null -X POST $GATEWAY/v1/chat \\"
cmd "     -H 'content-type: application/json' \\"
cmd "     -d '{\"prompt\":\"...\",\"max_tokens\":24}' | grep -i x-switchyard"
read -r first policy failovers <<<"$(ask)" || fail 'the first request did not complete'
[ -n "${first:-}" ] || fail 'no provider header came back; is the gateway healthy?'
say ""
say "   ${green}X-Switchyard-Provider: ${first}${reset}"
say "   X-Switchyard-Policy:   ${policy}"
say "   X-Switchyard-Failovers: ${failovers}"
say ""
say "   ${bold}${first}${reset} answered, with ${failovers} failover(s)."

# --- 3. break exactly that provider ------------------------------------------
step "3. Break ${first} -- the one that just answered. It now returns 503 to every call."
cmd "curl -sS -X POST $GATEWAY/admin/inject -H 'content-type: application/json' \\"
cmd "     -d '{\"provider\":\"${first}\",\"mode\":\"error\",\"rate\":1}'"
inject "$first" error || fail "could not inject a fault into ${first}"
say "   ${yellow}${first} is now failing every request.${reset}"

# --- 4. ask again, unchanged --------------------------------------------------
step '4. Send the identical request again. Nothing about the client changed.'
read -r second policy2 failovers2 <<<"$(ask)" || fail 'the second request did not complete'
[ -n "${second:-}" ] || fail 'no provider header on the second request'
say ""
say "   ${green}X-Switchyard-Provider: ${second}${reset}"
say "   X-Switchyard-Policy:   ${policy2}"
say "   X-Switchyard-Failovers: ${failovers2}"

# --- 5. the point -------------------------------------------------------------
say ""
if [ "$second" != "$first" ]; then
	say "${green}${bold}Failover.${reset} ${first} -> ${second}, same prompt, HTTP 200, no client change."
	say "The caller never saw the 503: the header is written only after a provider"
	say "accepted the request, so the gateway was still free to change its mind."
else
	say "${yellow}Both requests were served by ${second}.${reset}"
	say "That is worth reading, not a script bug: a breaker needs a few failures"
	say "before it opens. Rerun the last two steps, or run 'make demo' again."
fi

step '5. Put it back.'
cmd "make reset"
for p in apex bargain local; do inject "$p" healthy; done
say "   all providers healthy again."
say ""
say "${dim}The same failover in aggregate, with latency and spend: http://localhost:${GRAFANA_PORT:-3000}${reset}"
