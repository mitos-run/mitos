package compose

import "context"

// UnavailableBackend is the fail-closed default Backend. It represents a sandbox
// where the in-guest privileged dockerd plus docker compose runtime (issues #489
// and #490) is not present, which is the case today: that backend is a separate,
// hardware/kernel-gated follow-up. Every operation returns ErrBackendUnavailable
// and Available reports false, so the Provider advertises docker_compose=false
// and never claims compose works when it does not.
type UnavailableBackend struct{}

var _ Backend = UnavailableBackend{}

// Available reports false: no compose runtime is wired.
func (UnavailableBackend) Available() bool { return false }

// ServiceExec fails closed.
func (UnavailableBackend) ServiceExec(context.Context, ServiceExecRequest) (ExecResult, error) {
	return ExecResult{}, ErrBackendUnavailable
}

// ServiceDownloadFile fails closed.
func (UnavailableBackend) ServiceDownloadFile(context.Context, string, string) ([]byte, error) {
	return nil, ErrBackendUnavailable
}

// ServiceDownloadDir fails closed.
func (UnavailableBackend) ServiceDownloadDir(context.Context, string, string, []string) ([]byte, error) {
	return nil, ErrBackendUnavailable
}

// ServiceIsDir fails closed.
func (UnavailableBackend) ServiceIsDir(context.Context, string, string) (bool, error) {
	return false, ErrBackendUnavailable
}

// StopService fails closed.
func (UnavailableBackend) StopService(context.Context, ServiceStopRequest) error {
	return ErrBackendUnavailable
}
