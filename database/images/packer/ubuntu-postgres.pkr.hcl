packer {
  required_version = ">= 1.10.0"
  required_plugins {
    qemu = {
      version = ">= 1.0.9"
      source  = "github.com/hashicorp/qemu"
    }
  }
}

variable "build_date"           { default = "dev" }
variable "pg_versions"          { default = "15 16 17" }
variable "os_version"           { default = "22.04" }
variable "iso_url"              { default = "" }
variable "iso_checksum" {
  default = ""
  # build.sh already refuses to invoke Packer without a checksum (see its
  # images.yaml validation), but this catches a direct `packer build` call
  # that bypasses the wrapper script — Packer silently skips base-image
  # integrity verification when this is empty.
  validation {
    condition     = length(var.iso_checksum) > 0
    error_message = "The iso_checksum variable must be set — refusing to build without base-image integrity verification."
  }
}
variable "ssh_private_key_file" { default = env("PACKER_SSH_PRIVATE_KEY_FILE") }

locals {
  os_short   = replace(var.os_version, ".", "")
  image_name = "ubuntu-${local.os_short}-postgres-v${var.build_date}"
  output_dir = "output-${local.image_name}"
}

source "qemu" "ubuntu-postgres" {
  iso_url      = var.iso_url
  iso_checksum = var.iso_checksum
  disk_image   = true

  cd_files = ["./http/meta-data", "./http/user-data"]
  cd_label = "cidata"

  output_directory = local.output_dir
  vm_name          = "${local.image_name}.qcow2"
  format           = "qcow2"

  memory           = 2048
  cpus             = 2
  disk_size        = "20480"
  disk_interface   = "virtio"
  disk_compression = true
  accelerator      = "kvm"
  headless         = true
  net_device       = "virtio-net"

  communicator         = "ssh"
  ssh_username         = "ubuntu"
  ssh_private_key_file = var.ssh_private_key_file
  ssh_timeout          = "15m"
  boot_wait            = "30s"
  boot_command         = []
  shutdown_command     = "sudo shutdown -h now"
}

build {
  sources = ["source.qemu.ubuntu-postgres"]

  provisioner "shell" {
    inline = ["sudo cloud-init status --wait"]
  }

  provisioner "shell" {
    script            = "./scripts/provision.sh"
    environment_vars  = ["PG_VERSIONS=${var.pg_versions}"]
    max_retries       = 1
  }
}
