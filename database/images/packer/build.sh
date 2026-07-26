#!/bin/bash
# Usage: ./build.sh <build_date> [pg_versions] [os_version]
# Example: ./build.sh 20260615
#          ./build.sh 20260615 "15 16 17"
#          ./build.sh 20260615 "16 17 18" "24.04"
set -euo pipefail

BUILD_DATE="${1:?Usage: ./build.sh <build_date> [pg_versions] [os_version]}"
PG_VERSIONS="${2:-15 16 17}"
OS_VERSION="${3:-22.04}"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
IMAGES_YAML="$SCRIPT_DIR/images.yaml"
USER_DATA="$SCRIPT_DIR/http/user-data"
# A private, unpredictable directory rather than a PID-based /tmp path
# (CWE-377): mktemp -d creates it atomically with 0700 perms, so another
# user on the build host can't pre-place a symlink at a guessable name.
# ssh-keygen still needs the destination file itself to not already exist
# (it prompts to overwrite otherwise), so the key file lives inside this
# dir rather than being the mktemp target directly.
KEY_DIR="$(mktemp -d)"
KEY_FILE="$KEY_DIR/packer_key"

# Install yq if not present
if ! command -v yq &>/dev/null; then
  echo "==> yq not found — installing..."
  YQ_VERSION="v4.44.3"
  sudo wget -q -O /usr/local/bin/yq \
    "https://github.com/mikefarah/yq/releases/download/${YQ_VERSION}/yq_linux_amd64"
  sudo chmod +x /usr/local/bin/yq
  echo "==> yq $(yq --version) installed"
fi

# Validate OS version and read ISO details from images.yaml
OS_EOL=$(yq ".os_streams.\"${OS_VERSION}\".eol" "$IMAGES_YAML")
if [[ "$OS_EOL" == "null" || -z "$OS_EOL" ]]; then
  echo "ERROR: OS '${OS_VERSION}' not found in images.yaml — add it before building"
  exit 1
fi
TODAY=$(date +%Y-%m-%d)
if [[ "$TODAY" > "$OS_EOL" ]]; then
  echo "ERROR: Ubuntu ${OS_VERSION} reached EOL on ${OS_EOL} — do not build new images for this stream"
  exit 1
fi
echo "==> OS: Ubuntu ${OS_VERSION} (EOL: ${OS_EOL}) — OK"

ISO_URL=$(yq ".os_streams.\"${OS_VERSION}\".iso_url" "$IMAGES_YAML")
ISO_CHECKSUM=$(yq ".os_streams.\"${OS_VERSION}\".checksum_url" "$IMAGES_YAML")
if [[ "$ISO_CHECKSUM" == "null" || -z "$ISO_CHECKSUM" ]]; then
  echo "ERROR: OS '${OS_VERSION}' has no checksum_url in images.yaml — refusing to build without base-image integrity verification"
  exit 1
fi

# Validate PG versions from images.yaml
echo "==> Validating PG versions against EOL policy (today: $TODAY)"
for ver in $PG_VERSIONS; do
  eol=$(yq ".pg_versions.\"${ver}\".eol" "$IMAGES_YAML")
  if [[ "$eol" == "null" || -z "$eol" ]]; then
    echo "ERROR: PG $ver not found in images.yaml — add it before building"
    exit 1
  fi
  if [[ "$TODAY" > "$eol" ]]; then
    echo "ERROR: PG $ver reached EOL on $eol — remove it from PG_VERSIONS before building"
    exit 1
  fi
  echo "  PG $ver: EOL $eol — OK"
done

# Install Packer if not present
if ! command -v packer &>/dev/null; then
  echo "==> Packer not found — installing..."
  PACKER_VERSION="1.11.2"
  TMP_ZIP="/tmp/packer_${PACKER_VERSION}.zip"
  wget -q -O "$TMP_ZIP" \
    "https://releases.hashicorp.com/packer/${PACKER_VERSION}/packer_${PACKER_VERSION}_linux_amd64.zip"
  sudo unzip -q "$TMP_ZIP" -d /usr/local/bin/
  rm -f "$TMP_ZIP"
  echo "==> Packer $(packer version) installed"
fi

# Install QEMU if not present
if ! command -v qemu-system-x86_64 &>/dev/null; then
  echo "==> QEMU not found — installing..."
  sudo apt-get update -y -qq
  sudo apt-get install -y -qq qemu-system-x86 qemu-utils ovmf
fi

cleanup() {
  sed -i "s|ssh-ed25519 .*packer-build|PACKER_SSH_PUBLIC_KEY_PLACEHOLDER|g" \
    "$USER_DATA" 2>/dev/null || true
  rm -rf "$KEY_DIR"
}
trap cleanup EXIT

ssh-keygen -t ed25519 -f "$KEY_FILE" -N "" -C "packer-build" -q

sed -i "s|PACKER_SSH_PUBLIC_KEY_PLACEHOLDER|$(cat ${KEY_FILE}.pub)|g" \
  "$USER_DATA"

export PACKER_SSH_PRIVATE_KEY_FILE="$KEY_FILE"

cd "$SCRIPT_DIR"
packer init ubuntu-postgres.pkr.hcl

OS_SHORT="${OS_VERSION/./}"
packer build \
  -var "build_date=${BUILD_DATE}" \
  -var "pg_versions=${PG_VERSIONS}" \
  -var "os_version=${OS_VERSION}" \
  -var "iso_url=${ISO_URL}" \
  -var "iso_checksum=${ISO_CHECKSUM}" \
  ubuntu-postgres.pkr.hcl

echo "Built: output-ubuntu-${OS_SHORT}-postgres-v${BUILD_DATE}/ubuntu-${OS_SHORT}-postgres-v${BUILD_DATE}.qcow2"
