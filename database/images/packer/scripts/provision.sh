#!/bin/bash
set -euo pipefail

PG_VERSIONS="${PG_VERSIONS:-15 16 17}"

export DEBIAN_FRONTEND=noninteractive

# Security patches
sudo apt-get update -y
sudo apt-get upgrade -y

# Add PostgreSQL official apt repo
sudo apt-get install -y curl ca-certificates gnupg
sudo install -d /usr/share/postgresql-common/pgdg
curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc \
  | sudo gpg --dearmor \
  -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.gpg
sudo sh -c 'echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.gpg] \
  https://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" \
  > /etc/apt/sources.list.d/pgdg.list'
sudo apt-get update -y

# Install each PG version
PG_PKGS=""
for ver in $PG_VERSIONS; do
  PG_PKGS="$PG_PKGS postgresql-$ver postgresql-client-$ver"
done

sudo apt-get install -y \
  $PG_PKGS \
  postgresql-common \
  qemu-guest-agent \
  prometheus-postgres-exporter \
  jq

# Disable all PG clusters — bootstrap.sh activates the right one at boot
for ver in $PG_VERSIONS; do
  sudo systemctl disable "postgresql@${ver}-main" 2>/dev/null || true
done
sudo systemctl disable postgresql 2>/dev/null || true
sudo systemctl enable qemu-guest-agent

sudo apt-get clean
sudo rm -rf /var/lib/apt/lists/*

# Disable unattended-upgrades — security patches are delivered via controlled image
# rebuild and repave, not background apt on live VMs. Leaving this enabled causes:
#   - PostgreSQL minor-version drift between VMs provisioned at different times
#   - apt writes filling the COW delta, eroding storage savings
#   - Unexpected service restarts (needrestart) causing connection drops
sudo systemctl disable unattended-upgrades 2>/dev/null || true
sudo systemctl mask unattended-upgrades
sudo apt-get remove -y --purge unattended-upgrades 2>/dev/null || true

# Stop background apt timers that can wake up and write to the COW delta
sudo systemctl disable apt-daily.timer apt-daily-upgrade.timer 2>/dev/null || true
sudo systemctl mask apt-daily.service apt-daily-upgrade.service

# Seal the image — without this every VM looks like the same cloud-init instance
# and skips first-boot, so bootstrap.sh never runs
sudo cloud-init clean --logs --seed --machine-id
sudo truncate -s 0 /etc/machine-id

# Remove the Packer SSH key so it is not baked into customer VMs
sudo rm -f /home/ubuntu/.ssh/authorized_keys

sudo sync
