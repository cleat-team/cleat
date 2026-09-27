#!/bin/sh
# postremove: on purge only ($1 = purge), drop the config file, the system
# user and group this package created. An ordinary removal (upgrade or
# uninstall-but-keep-config) leaves all three alone.
set -e

if [ "$1" = "purge" ]; then
	rm -f /etc/cleat/cleat-worker.env
	rmdir /etc/cleat 2>/dev/null || true

	if getent passwd cleat >/dev/null 2>&1; then
		deluser cleat >/dev/null 2>&1 || true
	fi
	if getent group cleat >/dev/null 2>&1; then
		delgroup cleat >/dev/null 2>&1 || true
	fi

	if [ -d /run/systemd/system ]; then
		systemctl daemon-reload >/dev/null 2>&1 || true
	fi
fi

exit 0
