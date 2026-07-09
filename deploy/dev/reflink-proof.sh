#!/bin/sh
# Prove the rootfs-clone win from a reflink filesystem, on the KVM node (dev pod).
# FULL COPY (current node FS, prod today) vs FICLONE reflink clone on loopback XFS(reflink=1).
set -e
command -v mkfs.xfs >/dev/null 2>&1 || { apt-get update -qq && apt-get install -y -qq xfsprogs >/dev/null 2>&1; }
TMPL=/template-src/rootfs.ext4
ms() { date +%s%3N; }
echo "rootfs size: $(ls -lh "$TMPL" | awk '{print $5}')"
echo "=== BASELINE: full copy on the current node FS (what prod does today) ==="
sync; t0=$(ms); cp --reflink=auto "$TMPL" /var/lib/mitos/_fullclone.ext4; sync; t1=$(ms); echo "FULL_COPY_MS=$((t1 - t0))"; rm -f /var/lib/mitos/_fullclone.ext4
echo "=== REFLINK: loopback XFS(reflink=1) FICLONE ==="
rm -f /reflink.img; truncate -s 4G /reflink.img
mkfs.xfs -m reflink=1 -q /reflink.img
mkdir -p /reflink; mount -o loop /reflink.img /reflink
cp "$TMPL" /reflink/rootfs.ext4; sync
t0=$(ms); cp --reflink=always /reflink/rootfs.ext4 /reflink/_clone.ext4; sync; t1=$(ms); echo "REFLINK_CLONE_MS=$((t1 - t0))"
echo "space (shared extents => the clone adds ~0):"; df -h /reflink | tail -1
umount /reflink; rm -f /reflink.img
echo "REFLINK-PROOF-DONE"
