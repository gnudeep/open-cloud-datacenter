/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package harvester

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// buildNetworkData returns the cloud-init network-config v2 YAML for the
// VM's single data NIC. KubeVirt's cloudInitNoCloud datasource reads it
// from the Secret key `networkdata` and applies it at the `init-local`
// stage — before systemd-networkd starts — so the NIC has its IP, gateway
// and DNS before any module tries to talk to the network. (A write_files
// netplan stanza is too late: it lands during the `config` stage, after
// `apt update` has already failed for lack of routing.)
//
// When StaticNetwork is nil the NIC runs DHCP; when set, the supplied
// values are written as the static configuration.
func buildNetworkData(p VMCreateParams) string {
	if p.StaticNetwork == nil {
		return `version: 2
ethernets:
  data:
    match:
      driver: virtio_net
    dhcp4: true
`
	}
	ns := p.StaticNetwork
	search := ""
	if len(ns.SearchDomains) > 0 {
		search = fmt.Sprintf("\n      search: [%s]", strings.Join(ns.SearchDomains, ", "))
	}
	return fmt.Sprintf(`version: 2
ethernets:
  data:
    match:
      driver: virtio_net
    dhcp4: false
    addresses: [%s]
    routes:
      - to: default
        via: %s
    nameservers:
      addresses: [%s]%s
`,
		ns.Address,
		ns.Gateway,
		strings.Join(ns.Nameservers, ", "),
		search,
	)
}

func buildCloudInit(p VMCreateParams, adminPw, replPw, exporterPw string, tls *TLSBundle) string {
	backupConfig := "# backups disabled"
	if p.BackupEnabled && p.S3Config != nil {
		backupConfig = fmt.Sprintf(
			"S3_ENDPOINT=%s\n      S3_BUCKET=%s\n      S3_REGION=%s\n      S3_SECRET_REF=%s",
			p.S3Config.Endpoint,
			p.S3Config.Bucket,
			p.S3Config.Region,
			p.S3Config.SecretRef,
		)
	}

	vmUserBlock := ""
	if p.VMPassword != "" {
		vmUserBlock = fmt.Sprintf(`password: %s
chpasswd:
  expire: false
ssh_pwauth: true
`, p.VMPassword)
	}

	caCertB64 := base64.StdEncoding.EncodeToString([]byte(tls.CACertPEM))
	serverCertB64 := base64.StdEncoding.EncodeToString([]byte(tls.ServerCertPEM))
	serverKeyB64 := base64.StdEncoding.EncodeToString([]byte(tls.ServerKeyPEM))

	// Install everything from bootstrap.sh's apt calls rather than relying
	// on cloud-init's `packages:` module. Minimal cloud images (Ubuntu's
	// `ubuntu-24.04-minimal-cloudimg` is one) strip
	// `package_update_upgrade_install` from their cloud-init module list,
	// so a top-level `packages:` directive is silently ignored. Doing the
	// install from runcmd works on every flavour.
	return fmt.Sprintf(`#cloud-config
%swrite_files:
  - path: /etc/dbaas/bootstrap.env
    permissions: "0600"
    content: |
      INSTANCE_ID=%s
      DB_NAME=%s
      DB_PORT=%d
      MASTER_USER=%s
      MASTER_PASSWORD=%s
      REPL_PASSWORD=%s
      EXPORTER_PASSWORD=%s
      MAX_CONNECTIONS=%d
      %s
  - path: /etc/ssl/certs/pg-ca.crt
    encoding: b64
    permissions: "0644"
    content: %s
  - path: /etc/ssl/certs/pg-server.crt
    encoding: b64
    permissions: "0644"
    content: %s
  - path: /etc/ssl/private/pg-server.key
    encoding: b64
    permissions: "0600"
    content: %s
  - path: /etc/dbaas/bootstrap.sh
    permissions: "0700"
    content: |
      #!/bin/bash
      set -euo pipefail
      source /etc/dbaas/bootstrap.env

      # 1. Install PostgreSQL + helpers. Done here, not via cloud-init's
      #    "packages:" directive, so it works on minimal cloud images
      #    that don't load the package module.
      export DEBIAN_FRONTEND=noninteractive
      apt-get update -y
      apt-get install -y postgresql postgresql-contrib jq qemu-guest-agent prometheus-postgres-exporter
      systemctl enable --now qemu-guest-agent

      PG_VER=$(pg_lsclusters -h | awk '{print $1}' | head -1)
      PG_CONF="/etc/postgresql/${PG_VER}/main"

      # Move PostgreSQL data onto the dedicated pgdata disk before applying
      # DB-specific configuration. KubeVirt presents the VM's second disk as
      # /dev/vdb based on the VM spec's disk ordering: vda=os, vdb=pgdata,
      # vdc=cloud-init. If the disk is already formatted/mounted, preserve it.
      PGDATA_DEVICE="/dev/vdb"
      PGDATA_MOUNT="/var/lib/postgresql"
      if [ -b "${PGDATA_DEVICE}" ]; then
        systemctl stop postgresql || true
        if ! blkid "${PGDATA_DEVICE}" >/dev/null 2>&1; then
          mkfs.ext4 -F -L pgdata "${PGDATA_DEVICE}"
        fi
        PGDATA_UUID=$(blkid -s UUID -o value "${PGDATA_DEVICE}")
        mkdir -p /mnt/dbaas-pgdata
        if ! findmnt -n "${PGDATA_MOUNT}" >/dev/null 2>&1; then
          mount "${PGDATA_DEVICE}" /mnt/dbaas-pgdata
          # Copy the freshly-apt-installed cluster onto vdb on first boot.
          # The "is the disk a virgin cluster?" test is the absence of
          # PostgreSQL's own marker file (PG_VERSION), not "is the dir empty?":
          # mkfs.ext4 always creates lost+found, so a freshly-formatted disk
          # is never literally empty. On reboot the marker exists and we keep
          # the existing data.
          if [ ! -f "/mnt/dbaas-pgdata/${PG_VER}/main/PG_VERSION" ] && [ -d "${PGDATA_MOUNT}/${PG_VER}/main" ]; then
            cp -a "${PGDATA_MOUNT}/." /mnt/dbaas-pgdata/
          fi
          umount /mnt/dbaas-pgdata
          if ! grep -q "UUID=${PGDATA_UUID}[[:space:]]${PGDATA_MOUNT}[[:space:]]" /etc/fstab; then
            echo "UUID=${PGDATA_UUID} ${PGDATA_MOUNT} ext4 defaults,nofail 0 2" >> /etc/fstab
          fi
          mount "${PGDATA_MOUNT}"
        fi
        chown -R postgres:postgres "${PGDATA_MOUNT}"
      else
        echo "WARN: ${PGDATA_DEVICE} not found; PostgreSQL data remains on the OS disk" >&2
      fi

      # Fix server key ownership now that postgres user exists
      chown postgres:postgres /etc/ssl/private/pg-server.key

      # Listen on all interfaces and set the port
      sed -i "s/^#\?listen_addresses.*/listen_addresses = '*'/" "${PG_CONF}/postgresql.conf"
      sed -i "s/^#\?port.*/port = ${DB_PORT}/" "${PG_CONF}/postgresql.conf"
      sed -i "s/^#\?max_connections.*/max_connections = ${MAX_CONNECTIONS}/" "${PG_CONF}/postgresql.conf"

      # Enable SSL
      sed -i "s/^#\?ssl\b.*/ssl = on/" "${PG_CONF}/postgresql.conf"
      sed -i "s|^#\?ssl_cert_file.*|ssl_cert_file = '/etc/ssl/certs/pg-server.crt'|" "${PG_CONF}/postgresql.conf"
      sed -i "s|^#\?ssl_key_file.*|ssl_key_file = '/etc/ssl/private/pg-server.key'|" "${PG_CONF}/postgresql.conf"
      sed -i "s|^#\?ssl_ca_file.*|ssl_ca_file = '/etc/ssl/certs/pg-ca.crt'|" "${PG_CONF}/postgresql.conf"

      # SSL-only remote connections (hostssl rejects plain-text clients)
      echo "hostssl all all 0.0.0.0/0 scram-sha-256" >> "${PG_CONF}/pg_hba.conf"
      echo "hostssl replication all 0.0.0.0/0 scram-sha-256" >> "${PG_CONF}/pg_hba.conf"

      systemctl restart postgresql

      # Create admin user and database. The master user gets CREATEDB and
      # CREATEROLE so it can manage its own databases / roles, but NOT
      # SUPERUSER — RDS-style master users shouldn't be able to bypass
      # the engine's permission system. Database ownership is sufficient
      # for all in-database operations (DDL, GRANT, etc.).
      sudo -u postgres psql -p "${DB_PORT}" <<EOSQL
      DO \$\$
      BEGIN
        IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '${MASTER_USER}') THEN
          CREATE ROLE ${MASTER_USER} LOGIN CREATEDB CREATEROLE PASSWORD '${MASTER_PASSWORD}';
        END IF;
        IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'postgres_exporter') THEN
          CREATE ROLE postgres_exporter LOGIN PASSWORD '${EXPORTER_PASSWORD}';
        END IF;
      END \$\$;
      GRANT pg_monitor TO postgres_exporter;
      SELECT 'CREATE DATABASE ${DB_NAME} OWNER ${MASTER_USER}'
        WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = '${DB_NAME}')\gexec
      EOSQL

      cat >/etc/default/prometheus-postgres-exporter <<EOEXPORTER
      DATA_SOURCE_NAME=postgresql://postgres_exporter:${EXPORTER_PASSWORD}@127.0.0.1:${DB_PORT}/postgres?sslmode=require
      ARGS="--web.listen-address=:9187"
      EOEXPORTER
      # apt's postinst starts the exporter immediately with its default
      # (DATA_SOURCE_NAME-less) config, so 'systemctl enable --now' here is
      # a no-op against an already-running daemon and the new env file is
      # never read. An explicit restart is what actually picks it up.
      systemctl enable prometheus-postgres-exporter
      systemctl restart prometheus-postgres-exporter

      # Wipe the on-disk copy of the secrets now that PostgreSQL is
      # configured. The K8s Secret stays as the source of truth; leaving
      # bootstrap.env around lets anyone with root inside the VM (or
      # anyone who restores from a snapshot) read the admin password.
      shred -uz /etc/dbaas/bootstrap.env 2>/dev/null || rm -f /etc/dbaas/bootstrap.env
runcmd:
  - mkdir -p /var/lib/dbaas
  - chown root:root /var/lib/dbaas
  - /etc/dbaas/bootstrap.sh
final_message: "DBaaS bootstrap complete for %s"
`,
		vmUserBlock,
		p.ID,
		p.DBName,
		p.Port,
		p.MasterUser,
		adminPw,
		replPw,
		exporterPw,
		p.MaxConnections,
		backupConfig,
		caCertB64,
		serverCertB64,
		serverKeyB64,
		p.ID,
	)
}
