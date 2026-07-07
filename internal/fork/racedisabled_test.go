//go:build linux && !race

package fork

// raceDetectorEnabled is false in a normal (non -race) build, so the WP-handler
// KVM handshake tests run (as they do in the firecracker-test job).
const raceDetectorEnabled = false
