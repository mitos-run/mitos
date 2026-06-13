// Package ociroot pulls OCI images and flattens them into a directory tree
// and an ext4 rootfs image suitable for booting inside a microVM.
package ociroot

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	securejoin "github.com/cyphar/filepath-securejoin"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// mountPoints are the directories the guest agent expects to exist so it can
// mount the standard pseudo filesystems and the workspace volume.
var mountPoints = []string{"proc", "sys", "dev", "tmp", "run", "workspace"}

// secureJoin resolves entryName against destDir and returns an on-disk path
// guaranteed to stay inside destDir even when a parent component is an
// already-extracted symlink. It first rejects the obviously malicious shapes
// (absolute names, and names that lexically escape via "..") so callers get a
// hard error instead of a silently clamped path, then delegates to
// filepath-securejoin, which walks the remaining path on disk and resolves any
// existing symlink components against destDir. That on-disk walk is what closes
// the parent-symlink-traversal gap a purely lexical join cannot: an earlier
// entry that created a symlinked directory can no longer be written through.
// Image tars are untrusted input, so this is the single chokepoint every
// extracted path must pass through.
func secureJoin(destDir, entryName string) (string, error) {
	if filepath.IsAbs(entryName) {
		return "", fmt.Errorf("ociroot: absolute path entry rejected: %q", entryName)
	}
	if escapesLexically(destDir, filepath.Join(destDir, entryName)) {
		return "", fmt.Errorf("ociroot: path traversal entry rejected: %q", entryName)
	}
	joined, err := securejoin.SecureJoin(destDir, entryName)
	if err != nil {
		return "", fmt.Errorf("ociroot: secure join entry %q: %w", entryName, err)
	}
	if escapesLexically(destDir, joined) {
		return "", fmt.Errorf("ociroot: path traversal entry rejected: %q", entryName)
	}
	return joined, nil
}

// symlinkTargetStaysInside reports whether a symlink whose on-disk location is
// linkPath (already resolved through secureJoin) and whose stored target is
// target resolves to a location inside destDir when interpreted the way the
// kernel will at runtime: destDir is the rootfs root ("/"). An ABSOLUTE target
// is therefore resolved relative to destDir, not the host filesystem, exactly
// as the guest sees it once the rootfs is mounted as "/": absolute symlinks
// like /etc/ssl/certs/X.pem -> /usr/share/ca-certificates/... are normal and
// ubiquitous (ca-certificates, the alternatives system), not an escape.
// Rejecting them outright would break almost every real base image. A RELATIVE
// target resolves from the link's own directory. In BOTH cases the target is
// resolved through SecureJoin with destDir as the root, which clamps the walk
// to destDir, and the result is rejected if it still escapes. This is safe
// because every later file/hardlink write also goes through secureJoin /
// SecureJoin, so even a symlink pointing at "/" cannot let a subsequent entry
// write through it onto the host: SecureJoin treats that "/" as destDir.
func symlinkTargetStaysInside(destDir, linkPath, target string) bool {
	var walk string
	if filepath.IsAbs(target) {
		// Absolute target: rootfs-relative, i.e. relative to destDir.
		walk = filepath.Clean(target)
	} else {
		// Relative target: resolve from the link's own directory.
		linkDirRel, err := filepath.Rel(filepath.Clean(destDir), filepath.Dir(linkPath))
		if err != nil || linkDirRel == ".." || hasDotDotPrefix(linkDirRel) {
			return false
		}
		// Lexical check: a relative target may use ".." to climb within
		// destDir, but must not climb above it.
		if escapesLexically(destDir, filepath.Join(filepath.Dir(linkPath), target)) {
			return false
		}
		walk = filepath.Join(linkDirRel, target)
	}
	// On-disk check: SecureJoin resolves any symlinked parent against destDir
	// as the root, so the result cannot be redirected outside destDir.
	resolved, err := securejoin.SecureJoin(destDir, walk)
	if err != nil || escapesLexically(destDir, resolved) {
		return false
	}
	return true
}

// escapesLexically reports whether the cleaned path p falls outside destDir.
func escapesLexically(destDir, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(destDir), p)
	if err != nil {
		return true
	}
	return rel == ".." || hasDotDotPrefix(rel)
}

// hasDotDotPrefix reports whether p starts with a ".." path component.
func hasDotDotPrefix(p string) bool {
	return len(p) >= 3 && p[0] == '.' && p[1] == '.' && p[2] == os.PathSeparator
}

// ExtractImage flattens img with mutate.Extract and untars the result into
// destDir, preserving file modes, symlinks, and best-effort uid/gid. Every
// entry path is validated through secureJoin so malicious tars cannot write
// outside destDir.
func ExtractImage(img v1.Image, destDir string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("ociroot: create dest dir: %w", err)
	}

	rc := mutate.Extract(img)
	defer func() { _ = rc.Close() }()

	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("ociroot: read tar entry: %w", err)
		}
		if err := extractEntry(destDir, tr, hdr); err != nil {
			return err
		}
	}
	return nil
}

func extractEntry(destDir string, tr io.Reader, hdr *tar.Header) error {
	target, err := secureJoin(destDir, hdr.Name)
	if err != nil {
		return err
	}

	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(target, os.FileMode(hdr.Mode).Perm()); err != nil {
			return fmt.Errorf("ociroot: mkdir %q: %w", hdr.Name, err)
		}
		applyOwnership(target, hdr)

	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("ociroot: mkdir parent of %q: %w", hdr.Name, err)
		}
		if err := writeRegular(target, tr, os.FileMode(hdr.Mode).Perm()); err != nil {
			return fmt.Errorf("ociroot: write file %q: %w", hdr.Name, err)
		}
		applyOwnership(target, hdr)

	case tar.TypeSymlink:
		if !symlinkTargetStaysInside(destDir, target, hdr.Linkname) {
			return fmt.Errorf("ociroot: symlink %q -> %q escapes dest dir", hdr.Name, hdr.Linkname)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("ociroot: mkdir parent of symlink %q: %w", hdr.Name, err)
		}
		_ = os.Remove(target)
		if err := os.Symlink(hdr.Linkname, target); err != nil {
			return fmt.Errorf("ociroot: symlink %q: %w", hdr.Name, err)
		}

	case tar.TypeLink:
		linkTarget, err := secureJoin(destDir, hdr.Linkname)
		if err != nil {
			return fmt.Errorf("ociroot: hardlink %q target: %w", hdr.Name, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("ociroot: mkdir parent of hardlink %q: %w", hdr.Name, err)
		}
		_ = os.Remove(target)
		if err := os.Link(linkTarget, target); err != nil {
			return fmt.Errorf("ociroot: hardlink %q: %w", hdr.Name, err)
		}

	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		// Device, char, and fifo nodes need privilege we may not have and are
		// not needed for the rootfs use case. Log the skip with the safe path.
		log.Printf("ociroot: skipping special node %q (type %d)", hdr.Name, hdr.Typeflag)

	default:
		log.Printf("ociroot: skipping unsupported tar entry %q (type %d)", hdr.Name, hdr.Typeflag)
	}
	return nil
}

func writeRegular(target string, tr io.Reader, perm os.FileMode) error {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(f, tr); err != nil {
		return err
	}
	// OpenFile honors umask, so set the requested perm explicitly.
	if err := f.Chmod(perm); err != nil {
		return err
	}
	return f.Close()
}

// applyOwnership sets uid/gid best-effort. Failures (for example when running
// unprivileged) are intentionally ignored: the rootfs is still usable.
func applyOwnership(path string, hdr *tar.Header) {
	_ = os.Lchown(path, hdr.Uid, hdr.Gid)
}
