#!/usr/bin/env bash
set -euo pipefail

# ---------------------------------------------------------------------------
# reset-docker.sh — apt remove, clean, and reinstall Docker with containerd
# data on /localssd to keep the root filesystem lean.
# ---------------------------------------------------------------------------

CONTAINERD_DIR="/localssd/var/lib/containerd"
DOCKER_DIR="/var/lib/docker"

echo "=== 1. Stopping Docker and containerd ==="
sudo systemctl stop docker || true
sudo systemctl stop containerd || true

echo "=== 2. Removing Docker packages ==="
sudo apt remove --purge -y \
  docker-ce docker-ce-cli containerd.io \
  docker.io docker-compose docker-doc docker-registry \
  2>/dev/null || true

echo "=== 3. Cleaning residual package data ==="
sudo rm -rf /var/lib/docker      /var/lib/containerd \
            /etc/docker           /etc/containerd \
            /var/run/docker.sock  /run/containerd \
            ~/.docker             2>/dev/null || true

echo "=== 4. Removing stale containerd data on /localssd ==="
sudo rm -rf "$CONTAINERD_DIR"

echo "=== 5. Creating containerd directory tree on /localssd ==="
sudo mkdir -p "$CONTAINERD_DIR"/io.containerd.content.v1.content/{ingest,blobs/sha256}
sudo mkdir -p "$CONTAINERD_DIR"/io.containerd.snapshotter.v1.overlayfs/snapshots
sudo mkdir -p "$CONTAINERD_DIR"/io.containerd.metadata.v1.bolt
sudo mkdir -p "$CONTAINERD_DIR"/tmpmounts

echo "=== 6. Symlink /var/lib/containerd → $CONTAINERD_DIR ==="
sudo ln -sfn "$CONTAINERD_DIR" /var/lib/containerd

echo "=== 7. Reinstalling Docker ==="
sudo apt update -qq
sudo apt install -y \
  docker-ce docker-ce-cli containerd.io \
  docker-compose-plugin 2>/dev/null || \
sudo apt install -y \
  docker.io docker-compose-v2

echo "=== 8. Adding rcownie to docker group ==="
sudo usermod -aG docker rcownie 2>/dev/null || true

echo "=== 9. Verifying ==="
sudo systemctl status docker --no-pager | head -5
ls -la /var/lib/containerd
echo ""
echo "Done. Log out and back in (or run 'newgrp docker') for group membership to take effect."
