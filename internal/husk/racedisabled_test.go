//go:build linux && !race

package husk

// raceDetectorEnabled is false in a normal (non -race) build, so the live-cow
// source-arm KVM test runs (as it does in the firecracker-test job). It mirrors the
// internal/fork sibling: the WP handshake against a real Firecracker rides a live
// goroutine (Receive) and is timing-sensitive under the race detector, so the
// go-test job (which runs ./... -race) skips it and the firecracker-test job (no
// -race) proves it.
const raceDetectorEnabled = false
