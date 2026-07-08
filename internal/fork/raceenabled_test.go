//go:build linux && race

package fork

// raceDetectorEnabled is true when the test binary is built with -race. The
// WP-handler KVM handshake tests skip in this mode (their live-goroutine
// SCM_RIGHTS handshake is timing-sensitive under the detector); the
// firecracker-test job runs them without -race and proves them there.
const raceDetectorEnabled = true
