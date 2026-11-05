#!/bin/sh
# Stop it on removal, but not on an upgrade (rpm passes 1 then, deb passes "upgrade").
if [ -d /run/systemd/system ] && { [ "$1" = "0" ] || [ "$1" = "remove" ]; }; then
  systemctl disable --now dlna-bridge >/dev/null 2>&1 || true
fi
exit 0
