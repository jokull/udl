#!/bin/bash
# Deploy udl without ever leaving an unsigned binary on the live path.
#
# ~/bin/udl is shared: the daemon execs it, and so does every shadow agent that
# serves an NFS mount to Plex. Building straight onto that path is unsafe,
# because launchd's KeepAlive restarts an agent that exits — and a restarted
# agent execs whatever is there *now*. A binary whose signature was just
# replaced is ad-hoc signed, and an ad-hoc signature does not carry the TCC
# grant for /Volumes/Plex, so the mount would come up denied and Plex playback
# over the shadow would fail.
#
# So: build and sign a staging copy, then move it into place. rename(2) within
# a filesystem is atomic, so the live path holds a valid, signed binary at every
# instant. Running shadows still need a separate, idle remount to load it;
# activation reports those as pending instead of declaring deployment complete.
#
#   scripts/deploy.sh build      # build ~/bin/udl.new, print the sign command
#   scripts/deploy.sh activate   # swap it in and restart the daemon
#
# The signing step is deliberately manual: it needs the Keychain, which prompts.
set -euo pipefail

BIN="${UDL_BIN:-$HOME/bin/udl}"
STAGE="$BIN.new"
PLIST="${UDL_PLIST:-$HOME/Library/LaunchAgents/com.udl.daemon.plist}"
SIGN_ID="${UDL_SIGN_ID:-UDL}"

usage() {
    echo "usage: $0 {build|activate|status}" >&2
    exit 2
}

# is_adhoc reports whether a binary carries only an ad-hoc signature.
is_adhoc() {
    codesign -dv "$1" 2>&1 | grep -q 'Signature=adhoc'
}

cmd_build() {
    echo "building $STAGE"
    go build -o "$STAGE" ./cmd/udl
    echo
    echo "Built and unsigned. Sign it in a GUI terminal (the Keychain prompts):"
    echo
    echo "    codesign --force --sign \"$SIGN_ID\" \"$STAGE\""
    echo
    echo "then run: $0 activate"
}

cmd_activate() {
    [[ -f "$STAGE" ]] || { echo "no staged binary at $STAGE — run '$0 build' first" >&2; exit 1; }

    if ! codesign -dv "$STAGE" >/dev/null 2>&1; then
        echo "refusing to activate: $STAGE is not signed" >&2
        exit 1
    fi
    if is_adhoc "$STAGE"; then
        echo "refusing to activate: $STAGE is ad-hoc signed." >&2
        echo "An ad-hoc signature changes every build, so it does not carry the" >&2
        echo "existing TCC grant for /Volumes/Plex and the daemon and shadow" >&2
        echo "agents would lose access. Sign it with the '$SIGN_ID' identity:" >&2
        echo "    codesign --force --sign \"$SIGN_ID\" \"$STAGE\"" >&2
        exit 1
    fi

    echo "stopping the daemon"
    launchctl unload "$PLIST" 2>/dev/null || true

    # Atomic swap: the live path is never a partially written or unsigned file.
    mv -f "$STAGE" "$BIN"
    echo "installed $BIN ($(codesign -dv "$BIN" 2>&1 | grep -m1 '^Identifier=' || true))"

    echo "starting the daemon"
    launchctl load "$PLIST"

    sleep 2
    cmd_status
}

cmd_status() {
    local result=0
    if pgrep -f 'udl daemon' >/dev/null; then
        echo "daemon: running"
    else
        echo "daemon: NOT running"
        result=1
    fi

    # Check the shadow mounts themselves, discovered from the mount table rather
    # than assumed: the mount points are not where the Plex library paths suggest.
    local any=0
    while read -r m; do
        [[ -n "$m" ]] || continue
        any=1
        if probe_path "$m"; then
            echo "shadow:  $m responsive$(in_use_note "$m")"
        else
            echo "shadow:  $m NOT RESPONDING"
            result=1
        fi
    done < <(nfs_mounts)
    [[ $any -eq 1 ]] || echo "shadow:  no shadow mounts found"

    if is_adhoc "$BIN"; then
        echo "binary:  $BIN (AD-HOC — TCC grants will not persist)"
        result=1
    else
        echo "binary:  $BIN (signed)"
    fi

    shadow_versions || result=1
    return "$result"
}

# Replacing an executable does not update processes already running it. This
# used to leave shadows on old code for days while status said "responsive".
# A start time before the installed binary's mtime is a conservative signal
# that a restart is pending. Never restart here: random NFS handles cannot
# survive it, and live clients must first release the mount.
shadow_versions() {
    local installed listing pid status label started epoch result=0
    installed=$(stat -f '%m' "$BIN") || return 1
    listing=$(launchctl list) || return 1
    while read -r pid status label; do
        [[ "$label" == com.jokull.udl-shadow-* ]] || continue
        if [[ ! "$pid" =~ ^[0-9]+$ ]]; then
            echo "agent:   $label NOT running"
            result=1
            continue
        fi
        started=$(LC_ALL=C ps -p "$pid" -o lstart=) || started=""
        epoch=$(LC_ALL=C date -j -f '%a %b %e %T %Y' "$started" '+%s' 2>/dev/null) || epoch=""
        if [[ ! "$epoch" =~ ^[0-9]+$ ]]; then
            echo "agent:   $label start time unavailable; deployment not verified"
            result=1
        elif (( epoch < installed )); then
            echo "agent:   $label RESTART PENDING (process predates installed binary)"
            echo "         When idle: unmount normally, kickstart this agent, then verify a media read."
            result=1
        else
            echo "agent:   $label started after installed binary"
        fi
    done <<< "$listing"
    return "$result"
}

# nfs_mounts lists the shadow mounts, which are loopback NFS exports.
nfs_mounts() {
    mount | awk '/\(nfs/ && $1 ~ /^127\.0\.0\.1:/ { print $3 }'
}

# in_use_note reports whether anything has a file open under a mount, which is
# what decides whether restarting that shadow is disruptive. A restart gives the
# server a fresh, empty handle table, and go-nfs hands out a random UUID per
# path, so every handle a client already holds turns into ESTALE — a stream
# reading from the mount fails. Scoping lsof to the mount keeps this to a few
# hundred milliseconds; an unscoped lsof takes tens of seconds.
in_use_note() {
    local n output result=0
    output=$(bounded 8 lsof -n "$1" 2>&1) || result=$?
    # lsof returns 1 with no output for no matches. Timeouts, diagnostics,
    # and other failures do not establish that a mount is safe to restart.
    if (( result != 0 )) && ! { (( result == 1 )) && [[ -z "$output" ]]; }; then
        echo "  (open-file check failed; do not restart)"
        return
    fi
    n=$(printf '%s\n' "$output" | tail -n +2 | wc -l | tr -d ' ')
    if [[ "$n" -gt 0 ]]; then
        echo "  ⚠ $n open file(s) — restarting this shadow would disrupt them"
    else
        echo "  (nothing has it open: safe to restart)"
    fi
}

# bounded runs a command under a wall-clock alarm and returns its status, so a
# hung mount cannot stall this script.
bounded() {
    local secs="$1"
    shift
    if [[ -x /usr/bin/perl ]]; then
        # Do not inherit a locale that Perl cannot initialise. In particular,
        # launch/agent shells can carry C.UTF-8 even though macOS does not ship
        # that locale; Perl then exits before exec and every healthy mount is
        # reported as unresponsive.
        LC_ALL=C LANG=C /usr/bin/perl -e 'alarm shift; exec @ARGV' "$secs" "$@"
        return $?
    fi
    "$@"
}

# probe_path reports whether a path answers within a few seconds, so that a hung
# mount cannot stall this script. It must not rely on `timeout`: that is a shell
# function in an interactive session, not a program a script can exec, so a
# child shell fails with 127 and every mount looks dead.
probe_path() {
    bounded 5 ls "$1" >/dev/null 2>&1
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
    case "${1:-}" in
        build) cmd_build ;;
        activate) cmd_activate ;;
        status) cmd_status ;;
        *) usage ;;
    esac
fi
