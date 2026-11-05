#!/bin/sh
# Not started on install: the DVR's address is to be set first.
if [ -d /run/systemd/system ]; then
  systemctl daemon-reload >/dev/null 2>&1 || true
  if [ "$1" = "2" ] || [ "$1" = "configure" -a -n "$2" ]; then
    # an upgrade: restart it if it was running
    systemctl try-restart dlna-bridge >/dev/null 2>&1 || true
  fi
fi
echo "dlna-bridge installed. Set DLNA_DVR in /etc/dlna-bridge/dlna-bridge.env, then:"
echo "  sudo systemctl enable --now dlna-bridge"
exit 0
