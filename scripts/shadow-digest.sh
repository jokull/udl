#!/usr/bin/env bash
# shadow-digest.sh — one-screen health digest for the shadow libraries over
# the last N hours (default 24). Prints only what matters and exits nonzero
# when something needs attention, so a scheduled check costs one command.
#
#   scripts/shadow-digest.sh [hours]
#
# Sources: 'udl status' (agent health reports), the agents' logs (failed
# reads, reloads, revalidations), the daemon log (manifest rebuilds), the
# agent processes (unexpected restarts), and CI on main.
set -uo pipefail

HOURS="${1:-24}"
LOGS="$HOME/Library/Logs"
UDL="${UDL_BIN:-$HOME/bin/udl}"
SINCE="$(date -u -v-"${HOURS}"H +%Y-%m-%dT%H:%M:%S)"
problems=0
flag() { echo "  ✗ $*"; problems=$((problems + 1)); }

# Bound every external call: a hung mount or daemon must not hang the digest.
bounded() { /usr/bin/perl -e 'alarm shift; exec @ARGV' "$@"; }

echo "shadow digest — last ${HOURS}h (since ${SINCE}Z)"

echo "status:"
status="$(bounded 20 "$UDL" status 2>&1)" || flag "udl status failed: ${status%%$'\n'*}"
echo "$status" | grep -E "shadow" | sed 's/^ */  /'
echo "$status" | grep -qE "^ *[✗!] shadow:" && flag "an agent reports trouble (see above)"

# Check each mount from the client side, independent of what the agents
# report: an agent can be alive while the kernel's mount is dead.
echo "mounts:"
mounts="$(bounded 60 "$(dirname "$0")/deploy.sh" status 2>&1 | grep '^shadow:')"
echo "$mounts" | sed 's/^shadow: */  /'
grep -q "NOT RESPONDING" <<<"$mounts" && flag "a shadow mount is not responding"

echo "agents:"
for log in "$LOGS"/com.jokull.udl-shadow-*.log; do
    name="${log##*udl-shadow-}"; name="${name%.log}"
    pid="$(pgrep -f "udl shadow mount ${name} " | head -1)"
    if [[ -z "$pid" ]]; then
        flag "$name: no agent process"
        continue
    fi
    started="$(ps -o lstart= -p "$pid" | xargs -0 date -j -f "%a %b %e %T %Y " +%s 2>/dev/null || echo 0)"
    age_h=$(( ($(date +%s) - started) / 3600 ))
    # Lines this build writes start with an RFC3339 timestamp; compare as text.
    recent="$(awk -v s="$SINCE" 'substr($1,1,19) >= s' "$log")"
    fails="$(grep -c "read failed" <<<"$recent")"
    reloads="$(grep -c "manifest reloaded" <<<"$recent")"
    beat="$(grep "serving:" <<<"$recent" | tail -1 | sed -E 's/^[^ ]+ serving: //')"
    echo "  $name: pid $pid up ${age_h}h · ${reloads} reloads · ${fails} failed reads"
    [[ -n "$beat" ]] && echo "    last: $beat"
    if (( age_h < HOURS )); then
        flag "$name: agent restarted within the window (up ${age_h}h) — crash or manual restart?"
    fi
    if (( fails > 0 )); then
        flag "$name: $fails failed reads"
        grep "read failed" <<<"$recent" | sed -E 's/^([^ ]+) shadowfs: read failed /\1 /' \
            | awk '{print $2}' | sort | uniq -c | sed 's/^/      /'
        grep "read failed" <<<"$recent" | tail -3 | cut -c1-220 | sed 's/^/      /'
    fi
    grep -E "reload failed|reload skipped|health report not written" <<<"$recent" | tail -2 \
        | sed 's/^/    ! /' | grep . && flag "$name: reload or report trouble"
done

echo "rebuilds:"
rb="$(awk -v s="$SINCE" '{t=substr($1,6,19)} t >= s' "$LOGS/udl.log" | grep -E 'msg="shadow rebuil')"
if [[ -z "$rb" ]]; then
    flag "no manifest rebuild logged in the window (daemon rebuilds every 6h)"
else
    echo "$rb" | sed -E 's/^time=([^ ]+) level=[A-Z]+ msg="([^"]+)"/\1 \2/' | tail -6 | cut -c1-200 | sed 's/^/  /'
    grep -q 'shadow rebuild failed' <<<"$rb" && flag "a manifest rebuild failed"
fi

echo "ci (main):"
ci="$(bounded 20 gh run list -R jokull/udl -b main -L 1 --json conclusion,headSha,displayTitle \
    -q '.[0] | "\(if .conclusion == "" then "pending" else .conclusion end) \(.headSha[0:7]) \(.displayTitle)"' 2>/dev/null)"
echo "  ${ci:-unknown}"
[[ "$ci" == success* ]] || flag "CI on main is not green"

echo
if (( problems == 0 )); then
    echo "CLEAN"
else
    echo "ATTENTION: $problems issue(s)"
fi
exit $(( problems > 0 ))
