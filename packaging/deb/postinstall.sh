#!/bin/sh
# postinstall: goreleaser nfpms runs this after the .deb's files are laid down.
set -e

if ! getent group cleat >/dev/null 2>&1; then
	addgroup --system cleat
fi
if ! getent passwd cleat >/dev/null 2>&1; then
	adduser --system --ingroup cleat --no-create-home --shell /usr/sbin/nologin \
		--gecos "cleat durable workflow worker" cleat
fi

mkdir -p /etc/cleat
if [ ! -e /etc/cleat/cleat-worker.env ]; then
	cp /usr/share/cleat/cleat-worker.env.example /etc/cleat/cleat-worker.env
fi
chown root:cleat /etc/cleat/cleat-worker.env
chmod 640 /etc/cleat/cleat-worker.env

# Deliberately no `systemctl enable` and no `systemctl start`: the unit is
# disabled by default (cleat#2069) until an operator has set a database in
# the env file above.
if [ -d /run/systemd/system ]; then
	systemctl daemon-reload >/dev/null 2>&1 || true
fi

exit 0
