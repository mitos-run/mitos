package controlplane

import (
	"bytes"
	"encoding/json"
	"net/http"

	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/saas"
)

// errResp renders an apierr.Error as the public {"error": {...}} envelope and a
// ForwardResponse with the error's HTTP status. The token is never part of an
// error, so this is always safe to return to the caller.
func errResp(e apierr.Error) saas.ForwardResponse {
	var buf bytes.Buffer
	apierr.Encode(&statusRecorder{buf: &buf}, e)
	status := e.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	return saas.ForwardResponse{
		Status: status,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   buf.Bytes(),
	}
}

// jsonResp marshals payload and returns a ForwardResponse with the given status.
func jsonResp(status int, payload any) saas.ForwardResponse {
	b, err := json.Marshal(payload)
	if err != nil {
		return errResp(apierr.Get(apierr.CodeInternal).WithCause("could not encode the response"))
	}
	return saas.ForwardResponse{
		Status: status,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   b,
	}
}

// statusRecorder is a minimal http.ResponseWriter that captures only the body
// written by apierr.Encode (the status is taken from the Error directly).
type statusRecorder struct {
	buf    *bytes.Buffer
	header http.Header
}

func (s *statusRecorder) Header() http.Header {
	if s.header == nil {
		s.header = http.Header{}
	}
	return s.header
}
func (s *statusRecorder) Write(p []byte) (int, error) { return s.buf.Write(p) }
func (s *statusRecorder) WriteHeader(int)             {}

// withStatus returns a copy of e with its HTTP status overridden. apierr.Error
// has no WithStatus method, so the control plane sets the exported field on a
// copy. The status drives the gateway's echoed response code.
func withStatus(e apierr.Error, status int) apierr.Error {
	e.Status = status
	return e
}
