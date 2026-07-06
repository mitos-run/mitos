package controller

import (
	"strings"
	"testing"
)

// TestHuskForkRootfsInPodPathIsFrozenSnapshotCopy locks the controller's choice
// of clone source for a fork child: it must be the FROZEN source rootfs the
// source stub captured inside the fork snapshot's paused window (rootfs.ext4 next
// to mem+vmstate on the read-only snapshot mount), NOT the source's LIVE
// per-activation rootfs under the husk-rootfs CoW dir. Cloning from the live
// rootfs would let the resumed source drift the child's disk out of sync with the
// child's restored memory checkpoint (silent fs corruption); the frozen copy is
// the point-in-time pair.
func TestHuskForkRootfsInPodPathIsFrozenSnapshotCopy(t *testing.T) {
	got := huskForkRootfsInPodPath()

	if want := huskSnapshotMountPath + "/rootfs.ext4"; got != want {
		t.Fatalf("fork child clone source = %q, want the frozen snapshot rootfs %q", got, want)
	}
	// It must live on the snapshot mount (the mem+vmstate pair), so the frozen
	// disk is captured at the same instant as the memory checkpoint.
	if !strings.HasPrefix(got, huskSnapshotMountPath+"/") {
		t.Fatalf("fork child clone source %q must ride the snapshot mount %q", got, huskSnapshotMountPath)
	}
	// It must NOT be the source's live rootfs under the husk-rootfs CoW dir: a
	// resumed source keeps writing that file, which would corrupt the child clone.
	if strings.HasPrefix(got, huskRootfsCoWMountPath+"/") {
		t.Fatalf("fork child clone source %q must not be the source's live rootfs under %q", got, huskRootfsCoWMountPath)
	}
}
