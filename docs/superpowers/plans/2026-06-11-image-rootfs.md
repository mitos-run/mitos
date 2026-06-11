# Image to Rootfs Pipeline Implementation Plan (issue #10)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close issue #10. Build a bootable Firecracker rootfs from an OCI image reference (e.g. `python:3.12-slim`) on a real node: pull the image, flatten its layers, inject the guest agent as init, produce an ext4 image, then run the template's init commands inside the booted VM via the guest agent before snapshotting. This makes `SandboxTemplate.spec.image` real on the KVM engine (today the engine treats `image` as a rootfs file path and the only working path is a hand-built rootfs). Verified in KVM CI by building a rootfs from a small public image and booting it.

**Architecture:** A new package `internal/ociroot` that takes a `v1.Image` (pulled separately so the build is unit-testable without a registry), flattens it to a directory tree via go-containerregistry's `mutate.Extract`, injects the guest agent binary as `/init` plus a minimal `/bin/sh`, and produces an ext4 image using `mke2fs -d` (populates an ext4 from a directory with no mount and no root). A thin `PullImage(ref)` wraps the anonymous registry pull (the network part, exercised only in CI). The engine's `CreateTemplate` is rewired: when given an OCI ref it builds the rootfs via `ociroot`, then proceeds with the existing boot path, and runs the template init commands inside the VM via the guest agent (replacing the blind `initWaitSeconds` sleep) before pausing and snapshotting.

**Dependency:** `github.com/google/go-containerregistry` (pure Go, well-maintained, anonymous public-registry pulls + layer flattening). Add to go.mod.

**Context for the implementer:**
- Template build today: `internal/firecracker/template.go` `CreateTemplate(id, cfg VMConfig, initWaitSeconds int)` copies `cfg.RootfsPath` into the template dir, boots, sleeps `initWaitSeconds`, pauses, snapshots. `internal/fork/engine.go` `CreateTemplate(id, rootfsPath, initWaitSecs)` calls it. The daemon CreateTemplate RPC passes the image string as rootfsPath (`grpc_service.go`). The pool reconciler sends `template.Spec.Image`.
- Guest agent: `guest/agent/main.go` runs as PID 1 (`init=/init`), mounts proc/sys/dev/tmp, sets up /bin/sh, listens on vsock. The injected agent IS the new rootfs's /init. The CI's existing agent-rootfs build (in `.github/workflows/kvm-test.yaml`) shows the minimal rootfs layout (agent as /init + busybox for /bin/sh).
- Guest agent exec over vsock: `internal/vsock` Client.Exec, used by `cmd/test-agent`. The template-build init runs commands this way after boot.
- The CI runner has `mke2fs`/`mkfs.ext4` from e2fsprogs (`mkfs.ext4 -d <dir> <image> <blocks>` populates without mount).
- Conventions: CLAUDE.md authoritative. No em/en dashes. TDD. Explicit-path git add. Conventional commits, `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`. Lint darwin + GOOS=linux.

---

### Task 1: `internal/ociroot` build a rootfs dir + ext4 from a v1.Image

**Files:** Create `internal/ociroot/extract.go`, `internal/ociroot/ext4.go`, `internal/ociroot/pull.go`, and `_test.go`.

- [ ] go.mod: `go get github.com/google/go-containerregistry@latest`; `go mod tidy`.
- [ ] `extract.go`: `func ExtractImage(img v1.Image, destDir string) error` using `mutate.Extract(img)` to get the flattened tar reader, untar into destDir preserving modes, symlinks, and (best-effort) ownership, with a path-traversal guard (reject entries that escape destDir, including via `..` and absolute symlink targets) since image tars are untrusted input. `func InjectAgent(destDir, agentBinPath string) error` copies the agent binary to `destDir/init` (mode 0755) and ensures `destDir/bin/sh` exists (if the image lacks a shell, symlink or copy a static busybox; accept a busybox path param or skip if /bin/sh already present), plus mkdir the mount points the agent needs (/proc /sys /dev /tmp /run /workspace). 
- [ ] `ext4.go`: `func BuildExt4(srcDir, outPath string, sizeMB int) error` runs `mkfs.ext4 -F -q -d <srcDir> <outPath> <sizeMB>M` (or `mke2fs -t ext4 -d`); inject the runner via a function field for testability. `func DirSizeMB(dir) (int, error)` to pick a size with headroom (content size * 1.4 + a floor). TDD: BuildExt4 issues the right mke2fs argv (recording runner); size calc has headroom and a minimum.
- [ ] `pull.go`: `func PullImage(ctx, ref string) (v1.Image, error)` using `remote.Image(name.ParseReference(ref), remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain))` for anonymous public pulls (auth keychain still works for private if configured). This is the network part, NOT unit-tested (CI/integration only).
- [ ] Tests: build an in-memory image with `github.com/google/go-containerregistry/pkg/v1/random` or by appending a tar layer via `crane.Append`/`tarball`, ExtractImage it to a temp dir, assert the files land with correct content and the traversal guard rejects a malicious `../escape` entry (craft a layer with a `../` path and assert ExtractImage errors). InjectAgent puts a fake agent at destDir/init 0755 and creates the mount dirs. BuildExt4 argv via recording runner. No real mke2fs or registry in unit tests.
- [ ] Commit `feat: internal/ociroot pulls and flattens OCI images into an ext4 rootfs`.

### Task 2: engine CreateTemplate builds from an image ref; init commands run in the VM

**Files:** `internal/firecracker/template.go`, `internal/fork/engine.go`, `internal/daemon/grpc_service.go` (CreateTemplate plumbing if the init commands need to flow), proto if init commands are not yet plumbed, `cmd/forkd/main.go`, tests.

- [ ] Distinguish an OCI ref from a rootfs file path: if `image` looks like an OCI reference (parses via name.ParseReference and the path does not exist as a file) build via ociroot; if it is an existing file path, keep the current copy behavior (back-compat for the hand-built CI rootfs and tests). Document the heuristic.
- [ ] forkd flags: `--agent-bin` (path to the guest agent binary to inject as /init, required for image builds) and optionally `--busybox-bin` (a static /bin/sh source if images lack a shell). EngineOpts plumbed. Note in the daemonset that the agent binary must be present in the forkd image (a follow-up can `go:embed` it; for now mount/ship it).
- [ ] CreateTemplate: when building from an image, `PullImage` -> `ExtractImage` to a temp dir -> `InjectAgent` -> `BuildExt4` into the template's rootfs path, then proceed to boot. After boot, instead of `time.Sleep(initWaitSeconds)`, connect to the guest agent over vsock and run each `init` command via Exec, failing the template build if any init command exits nonzero (a template whose `pip install` failed must not be snapshotted and served). Then pause + snapshot as today. This makes `init: [...]` real. The init commands must be plumbed from `template.Spec.Init` through the CreateTemplate RPC to the engine; check the proto CreateTemplateRequest has init_commands (it does) and wire it end to end.
- [ ] TDD: the image-vs-path heuristic (unit); the init-command-runs-in-VM path is KVM-only (Task 3 CI), but unit-test that CreateTemplate returns an error when an init command would fail by seaming the agent-exec call (a fake exec returning nonzero makes the build fail). Keep existing template tests (file-path rootfs) green.
- [ ] Commit `feat: engine builds templates from OCI images and runs init in the VM`.

### Task 3: KVM CI proof + docs + PR

**Files:** `.github/workflows/kvm-test.yaml`, `docs/templates.md` (new), `ROADMAP.md`, full verification.

- [ ] KVM CI phase: build forkd + the guest agent; use `cmd/forkd` or a small helper or `cmd/sandbox-server` to CreateTemplate from a TINY public OCI image (e.g. `busybox:stable` or `alpine:3.20`, small and reliable; pull is rerunnable on Docker Hub flake) with an init command (e.g. `["echo built > /built.txt"]`), then fork a sandbox from that template and exec `cat /built.txt` over the guest agent, asserting the init command ran and the image filesystem is present (e.g. busybox binaries exist). This proves the whole pipeline: OCI pull -> ext4 -> boot -> init-in-VM -> snapshot -> fork -> exec. Gate on success. The image pull may flake on Docker Hub; wrap the pull in a retry and document that a registry mirror is the production answer.
- [ ] `docs/templates.md`: how a template is built from an image (pull, flatten, inject agent, ext4, boot, run init, snapshot), the OCI-ref-vs-file-path heuristic, the agent-binary requirement, init-command semantics (run in the VM, failure aborts the build), and what is OPEN (go:embed the agent into forkd; registry mirror/credentials for private images; layer caching to speed pool builds; non-ext4 backends).
- [ ] ROADMAP: flip the image->rootfs pipeline line to done; note the open follow-ups.
- [ ] Full verification (build darwin + GOOS=linux, vet, lint both, all Go suites with envtest, Python suite, gofmt zero, dash grep zero, YAML parse, go.mod/go.sum committed).
- [ ] Push `feat/image-rootfs`, PR `Image to rootfs: build Firecracker templates from OCI images` body Closes #10, watch CI (confirm the pull+build+boot+init phase passes), rerun Docker Hub pull flakes, merge when green.

**Out of scope (follow-ups):** go:embed the guest agent into the forkd binary so no external agent path is needed; OCI layer caching / incremental rebuilds (ties into the CAS store and pool-build speed); registry credentials and private images; non-ext4 backends (erofs, virtio-fs); running init commands with the full network/secret context (init runs at build time, before claim-time secrets, by design).
