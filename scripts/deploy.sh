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
# instant and a restart during the deploy is harmless.
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
    if pgrep -f 'udl daemon' >/dev/null; then
        echo "daemon: running"
    else
        echo "daemon: NOT running"
    fi

    # Check the shadow mounts themselves, discovered from the mount table rather
    # than assumed: the mount points are not where the Plex library paths suggest.
    local any=0
    while read -r m; do
        [[ -n "$m" ]] || continue
        any=1
        if probe_path "$m"; then
            echo "shadow:  $m responsive"
        else
            echo "shadow:  $m NOT RESPONDING"
        fi
    done < <(nfs_mounts)
    [[ $any -eq 1 ]] || echo "shadow:  no shadow mounts found"

    if is_adhoc "$BIN"; then
        echo "binary:  $BIN (AD-HOC — TCC grants will not persist)"
    else
        echo "binary:  $BIN (signed)"
    fi
}

# nfs_mounts lists the shadow mounts, which are loopback NFS exports.
nfs_mounts() {
    mount | awk '/\(nfs/ && $1 ~ /^127\.0\.0\.1:/ { print $3 }'
}

# probe_path reports whether a path answers within a few seconds, so that a hung
# mount cannot stall this script. It must not rely on `timeout`: that is a shell
# function in an interactive session, not a program a script can exec, so a
# child shell fails with 127 and every mount looks dead.
probe_path() {
    if command -v timeout >/dev/null 2>&1 && [[ -x "$(command -v timeout)" ]]; then
        timeout 5 ls "$1" >/dev/null 2>&1
        return $?
    fi
    if [[ -x /usr/bin/perl ]]; then
        /usr/bin/perl -e 'alarm shift; exec @ARGV' 5 ls "$1" >/dev/null 2>&1
        return $?
    fi
    ls "$1" >/dev/null 2>&1
}

case "${1:-}" in
    build) cmd_build ;;
    activate) cmd_activate ;;
    status) cmd_status ;;
    *) usage ;;
esac
