#!/usr/bin/env bash
# PVM no-nested-virt spike (issue #40), reproducible end to end.
#
# Provisions a PVM host kernel + a PVM-enlightened guest kernel on a plain cloud
# VPS that exposes NO /dev/kvm, then exercises the core mitos primitive path with
# the Loophole Labs PVM Firecracker fork: microVM boot, guest exec over the serial
# console, snapshot create, and snapshot restore.
#
# Run this ON the target host (a Hetzner cpx or any VPS with no vmx/svm), as root.
# It is intentionally a host-setup script, not the Go cmd/bench harness, because
# the thing under test here is the HOST substrate (the PVM kernel module), not the
# in-process fork engine. Once a PVM tier is real, cmd/bench runs on top of it.
#
# Result of the 2026-06-23 run on a Hetzner CPX22 (AMD EPYC, Ubuntu 26.04):
#   - host /dev/kvm appears under kvm_pvm:           PASS
#   - microVM boot + guest exec under PVM:           PASS
#   - Firecracker snapshot CREATE:                   PASS
#   - Firecracker snapshot RESTORE:                  FAIL (unhandled WRMSR
#     0xc0010007 = AMD MSR_K7_PERFCTR3; PVM module partial-writes the MSR set and
#     Firecracker aborts the restore). kvm enable_pmu=0 does NOT work around it.
# See docs/platforms/pvm-evaluation.md for the full writeup and decision impact.

set -euo pipefail

WORK=${WORK:-/root}
FC_FORK_URL="https://github.com/loopholelabs/firecracker/releases/download/release-main-live-migration-pvm/firecracker.linux-x86_64"
PVM_HOST_KERNEL_OCI="ghcr.io/openfaasltd/actuated-kernel-pvm-host:x86_64-latest"
PVM_GUEST_BRANCH="pvm-612"
PVM_GUEST_CONFIG_URL="https://raw.githubusercontent.com/virt-pvm/misc/main/pvm-guest-6.12.33.config"
ALPINE_URL="https://dl-cdn.alpinelinux.org/alpine/v3.21/releases/x86_64/alpine-minirootfs-3.21.0-x86_64.tar.gz"

log() { printf '\n=== %s ===\n' "$*"; }

baseline() {
  log "host baseline (expect: no vmx/svm, no /dev/kvm)"
  uname -a
  grep -oE 'vmx|svm' /proc/cpuinfo | sort -u || echo "(no vmx/svm: no nested virt, as expected)"
  ls -l /dev/kvm 2>&1 || echo "(/dev/kvm absent, as expected)"
}

install_host_kernel() {
  log "install PVM host kernel 6.12.33"
  command -v arkade >/dev/null || curl -sLS https://get.arkade.dev | sh
  export PATH="$PATH:/usr/local/bin:$HOME/.arkade/bin"
  mkdir -p "$WORK/pvm" && cd "$WORK/pvm"
  arkade oci install "$PVM_HOST_KERNEL_OCI" --path "$WORK/pvm"
  dpkg -i "$WORK"/pvm/linux-image-6.12.33_*.deb
  # PVM needs page-table isolation off; default-boot the PVM entry.
  sed -i '/^GRUB_CMDLINE_LINUX_DEFAULT=/ { s/ pti=off//g; s/"$/ pti=off"/ }' /etc/default/grub
  sed -i 's/^GRUB_DEFAULT=.*/GRUB_DEFAULT=saved/' /etc/default/grub
  update-grub
  grub-set-default "Advanced options for Ubuntu>Ubuntu, with Linux 6.12.33"
  update-grub
  echo "Reboot into 6.12.33, then re-run this script with STEP=after-reboot."
}

after_reboot() {
  log "verify /dev/kvm under kvm_pvm (the headline run-anywhere result)"
  uname -r
  cat /proc/cmdline
  modprobe kvm_pvm
  lsmod | grep -i pvm
  ls -l /dev/kvm && echo "KVM PRESENT on a host with no hardware virt: PASS"
}

build_guest_kernel() {
  log "build PVM-enlightened guest vmlinux from $PVM_GUEST_BRANCH"
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq build-essential flex bison libssl-dev libelf-dev bc git curl cpio dwarves e2fsprogs
  curl -sSL -o "$WORK/pvm-guest.config" "$PVM_GUEST_CONFIG_URL"
  cd "$WORK"
  [ -d linux-pvm ] || git clone --depth 1 --branch "$PVM_GUEST_BRANCH" https://github.com/virt-pvm/linux.git linux-pvm
  cd linux-pvm
  cp "$WORK/pvm-guest.config" .config
  make olddefconfig
  make -j"$(nproc)" vmlinux
  ls -la vmlinux
}

build_rootfs() {
  log "assemble minimal ext4 rootfs with an exec-proving init"
  cd "$WORK" && mkdir -p rootfs-build/mnt && cd rootfs-build
  curl -sSL -o alpine.tar.gz "$ALPINE_URL"
  rm -f rootfs.ext4
  dd if=/dev/zero of=rootfs.ext4 bs=1M count=128 status=none
  mkfs.ext4 -q rootfs.ext4
  mount -o loop rootfs.ext4 mnt
  tar -xzf alpine.tar.gz -C mnt
  tee mnt/init >/dev/null <<'INIT'
#!/bin/sh
mount -t proc proc /proc
mount -t sysfs sys /sys
echo PVM_GUEST_EXEC_OK
echo guest-uname=$(uname -r)
echo guest-clocksource=$(cat /sys/devices/system/clocksource/clocksource0/current_clocksource)
echo guest-virtflag-count=$(grep -c -E '(vmx|svm)' /proc/cpuinfo)
i=0
while true; do echo PVM_HEARTBEAT=$i; i=$((i+1)); sleep 1; done
INIT
  chmod +x mnt/init
  umount mnt
  echo "rootfs.ext4 ready"
}

run_test() {
  log "boot + snapshot create + snapshot restore under PVM"
  cd "$WORK/fc" 2>/dev/null || { mkdir -p "$WORK/fc" && cd "$WORK/fc"; }
  [ -x ./firecracker ] || { curl -sSL -o firecracker "$FC_FORK_URL"; chmod +x firecracker; }
  ./firecracker --version | head -1
  cp -f "$WORK/rootfs-build/rootfs.ext4" "$WORK/fc/rootfs.ext4"
  modprobe kvm_pvm || true
  dmesg -C || true

  local K="$WORK/linux-pvm/vmlinux"
  api(){ curl -s --unix-socket "$WORK/fc/fc.sock" "$@"; }
  rm -f "$WORK"/fc/fc.sock "$WORK"/fc/boot.log "$WORK"/fc/snap.file "$WORK"/fc/mem.file
  ./firecracker --api-sock "$WORK/fc/fc.sock" > "$WORK/fc/boot.log" 2>&1 &
  local fcpid=$!; sleep 1
  api -X PUT http://localhost/boot-source -H 'Content-Type: application/json' \
    -d "{\"kernel_image_path\":\"$K\",\"boot_args\":\"console=ttyS0 reboot=k panic=1 pci=off init=/init root=/dev/vda rw\"}" >/dev/null
  api -X PUT http://localhost/drives/rootfs -H 'Content-Type: application/json' \
    -d "{\"drive_id\":\"rootfs\",\"path_on_host\":\"$WORK/fc/rootfs.ext4\",\"is_root_device\":true,\"is_read_only\":false}" >/dev/null
  api -X PUT http://localhost/machine-config -H 'Content-Type: application/json' \
    -d '{"vcpu_count":1,"mem_size_mib":256}' >/dev/null
  api -X PUT http://localhost/actions -H 'Content-Type: application/json' \
    -d '{"action_type":"InstanceStart"}' >/dev/null
  sleep 6
  grep -E 'PVM_GUEST_EXEC_OK|guest-|PVM_HEARTBEAT' "$WORK/fc/boot.log" | tail -8

  api -X PATCH http://localhost/vm -H 'Content-Type: application/json' -d '{"state":"Paused"}' >/dev/null
  api -X PUT http://localhost/snapshot/create -H 'Content-Type: application/json' \
    -d "{\"snapshot_type\":\"Full\",\"snapshot_path\":\"$WORK/fc/snap.file\",\"mem_file_path\":\"$WORK/fc/mem.file\"}" >/dev/null
  ls -la "$WORK"/fc/snap.file "$WORK"/fc/mem.file | awk '{print $5, $9}'
  echo "snapshot CREATE: PASS"
  kill "$fcpid" 2>/dev/null || true; sleep 1

  rm -f "$WORK"/fc/fc2.sock "$WORK"/fc/restore.log
  ./firecracker --api-sock "$WORK/fc/fc2.sock" > "$WORK/fc/restore.log" 2>&1 &
  local fc2=$!; sleep 1
  local resp
  resp=$(curl -s --unix-socket "$WORK/fc/fc2.sock" -X PUT http://localhost/snapshot/load \
    -H 'Content-Type: application/json' \
    -d "{\"snapshot_path\":\"$WORK/fc/snap.file\",\"mem_backend\":{\"backend_type\":\"File\",\"backend_path\":\"$WORK/fc/mem.file\"},\"resume_vm\":true}")
  if [ -z "$resp" ]; then
    echo "snapshot RESTORE: PASS"
    sleep 3
    grep -oE 'PVM_HEARTBEAT=[0-9]+' "$WORK/fc/restore.log" | tail -3
  else
    echo "snapshot RESTORE: FAIL -> $resp"
    dmesg | grep -i -E 'wrmsr|msr' | tail -3 || true
  fi
  kill "$fc2" 2>/dev/null || true
}

STEP=${STEP:-all}
case "$STEP" in
  baseline)      baseline ;;
  install)       baseline; install_host_kernel ;;
  after-reboot)  after_reboot; build_guest_kernel; build_rootfs; run_test ;;
  test)          run_test ;;
  all)           baseline; install_host_kernel ;;  # then reboot and run STEP=after-reboot
  *) echo "unknown STEP=$STEP (baseline|install|after-reboot|test)"; exit 2 ;;
esac
