#!/bin/sh
# preremove: stop the service before its unit file is removed. Runs on both
# a plain removal and a purge; does not touch /etc/cleat/cleat-worker.env or
# the cleat user -- a purge script handles that, and an upgrade must not.
set -e

if [ -d /run/systemd/system ]; then
	systemctl stop cleat-worker >/dev/null 2>&1 || true
fi

exit 0
