//go:build linux && race

package husk

// raceDetectorEnabled is true when the husk test binary is built with -race. The
// live-cow source-arm KVM test skips in this mode (its live-goroutine write-protect
// handshake against a real Firecracker is timing-sensitive under the detector); the
// firecracker-test job runs without -race and proves it there.
const raceDetectorEnabled = true
