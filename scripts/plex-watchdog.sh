#!/bin/bash
# Run periodically from a user LaunchAgent. LaunchServices starts Plex in its
# normal GUI session; launching its executable directly from launchd can give
# it a different macOS privacy context. Never kill an existing server or stream.
set -euo pipefail

if /usr/bin/pgrep -x 'Plex Media Server' >/dev/null; then
    exit 0
fi

echo "$(/bin/date -u '+%Y-%m-%dT%H:%M:%SZ') Plex server absent; launching"
/usr/bin/open -g -a 'Plex Media Server'
