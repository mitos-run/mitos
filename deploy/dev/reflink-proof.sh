#!/bin/sh
# Prove the rootfs-clone win from a reflink filesystem, on the KVM node.
# Compares a FULL COPY (current node FS, ~40ms/600MB) vs a true FICLONE reflink
# clone on a loopback XFS(reflink=1) (near-instant, shared extents).
set -e
command -v mkfs.xfs >/dev/null 2>&1 || { apt-get update -qq && apt-get install -y -qq xfsprogs >/dev/null 2>&1; }
TMPL=/template-src/rootfs.ext4
echo "rootfs size:"; ls -lh "$TMPL"
echo "=== BASELINE: full copy on the current node FS (what prod does today) ==="
sync; time cp --reflink=auto "$TMPL" /var/lib/mitos/_fullclone.ext4; rm -f /var/lib/mitos/_fullclone.ext4
echo "=== REFLINK: loopback XFS(reflink=1) FICLONE clone ==="
rm -f /reflink.img; truncate -s 4G /reflink.img
mkfs.xfs -m reflink=1 -q /reflink.img
mkdir -p /reflink; mount -o loop /reflink.img /reflink
cp "$TMPL" /reflink/rootfs.ext4; sync
echo "reflink clone time (cp --reflink=always):"
time cp --reflink=always /reflink/rootfs.ext4 /reflink/_clone.ext4
echo "space (shared extents => clone adds ~0):"; du -sh /reflink/rootfs.ext4 /reflink/_clone.ext4 2>/dev/null; df -h /reflink | tail -1
umount /reflink; rm -f /reflink.img
echo "REFLINK-PROOF-DONE"
