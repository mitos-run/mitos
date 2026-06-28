package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// pathUnder reports whether child is parent itself or lexically nested under it,
// after cleaning both. It decides whether forkd can bind ONE mount (the data dir)
// to keep the jailer's template hard-links CoW: only when the chroot base lives
// under the data dir do the per-VM chroots and the template files share a mount.
func pathUnder(child, parent string) bool {
	c := filepath.Clean(child)
	p := filepath.Clean(parent)
	return c == p || strings.HasPrefix(c, p+string(filepath.Separator))
}

// verifyChrootCoW probes whether the jailer will HARD-LINK (CoW) the template
// rootfs/snapshot into each per-VM chroot, or silently fall back to a full per-VM
// COPY. link(2) refuses to cross a mount boundary even when both paths are on one
// filesystem, so a chroot base on its own mount (separate from the data dir that
// holds the template files) forces a copy that defeats fork CoW and is slow
// enough to time the jailer out mid-build, leaving a stale chroot (issue #526).
//
// It creates a tiny temp file under dataDir, hard-links it under chrootBase, and
// reports cow=false with actionable remediation in detail when the link crosses a
// mount boundary (EXDEV). Both probe files are removed. A non-EXDEV failure is a
// real error. The probe writes only zero-length marker files and logs paths only.
func verifyChrootCoW(dataDir, chrootBase string) (cow bool, detail string, err error) {
	return verifyChrootCoWWithLink(dataDir, chrootBase, os.Link)
}

// verifyChrootCoWWithLink is verifyChrootCoW with an injectable link function so
// the EXDEV branch is unit-testable on a single filesystem.
func verifyChrootCoWWithLink(dataDir, chrootBase string, link func(oldname, newname string) error) (bool, string, error) {
	if dataDir == "" || chrootBase == "" {
		return true, "", nil
	}
	if err := os.MkdirAll(chrootBase, 0o755); err != nil {
		return false, "", fmt.Errorf("create chroot base %s for CoW probe: %w", chrootBase, err)
	}
	src, err := os.CreateTemp(dataDir, ".cow-probe-*")
	if err != nil {
		return false, "", fmt.Errorf("create CoW probe file under %s: %w", dataDir, err)
	}
	srcPath := src.Name()
	_ = src.Close()
	defer func() { _ = os.Remove(srcPath) }()

	dstPath := filepath.Join(chrootBase, "."+filepath.Base(srcPath)+".link")
	_ = os.Remove(dstPath)
	linkErr := link(srcPath, dstPath)
	if linkErr == nil {
		_ = os.Remove(dstPath)
		return true, "", nil
	}
	if errors.Is(linkErr, syscall.EXDEV) {
		detail := fmt.Sprintf("jailer --chroot-base %s and --data-dir %s are on different MOUNTS (not just one filesystem): the template rootfs/snapshot is COPIED into every VM chroot instead of CoW hard-linked, which defeats fork copy-on-write and can time the jailer out mid-build. Co-locate --chroot-base UNDER --data-dir (for example %s/jailer) so forkd binds one mount and links stay CoW (issue #526).", chrootBase, dataDir, dataDir)
		return false, detail, nil
	}
	return false, "", fmt.Errorf("CoW probe hard link %s into %s: %w", srcPath, chrootBase, linkErr)
}
