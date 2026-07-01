// This go.mod marks the vendored upstream agent-sandbox v0.4.6 artifacts as a
// SEPARATE nested module so the Go toolchain in the parent module (go build /
// go vet / go test ./...) does NOT try to compile them. This second pinned
// minor is vendored for the apply-unchanged conformance dimension only (CRDs +
// examples + extensions/examples); the only Go file that rides along in the
// upstream examples tree is build-tagged and is never compiled here. The nested
// module boundary keeps the parent build clean regardless.
//
// Do NOT add this module to a go.work or run go mod tidy against it; the
// upstream artifacts under this directory are vendored verbatim and must not be
// edited (apply-unchanged is the conformance point). See README.md and
// docs/facade-conformance.md.
module sigs.k8s.io/agent-sandbox

go 1.26.2
