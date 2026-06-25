package sandboxrpc

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"connectrpc.com/connect"

	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
	"mitos.run/mitos/proto/sandbox/v1/sandboxv1connect"
)

// vitalsGuest extends fakeGuest for Vitals operations. It holds scripted
// GuestVitals samples and records the interval it was called with.
type vitalsGuest struct {
	fakeGuest

	// gotInterval records the interval Vitals was called with.
	gotInterval time.Duration

	// samples is the scripted sequence of vitals samples the fake emits.
	samples []*sandboxv1.GuestVitals

	// openErr, when non-nil, is returned by Vitals instead of a stream.
	openErr error
}

// fakeVitalsStream backs vitalsGuest.Vitals.
type fakeVitalsStream struct {
	samples []*sandboxv1.GuestVitals
	pos     int
}

func (s *fakeVitalsStream) Recv() (*sandboxv1.GuestVitals, error) {
	if s.pos >= len(s.samples) {
		return nil, io.EOF
	}
	v := s.samples[s.pos]
	s.pos++
	return v, nil
}

func (s *fakeVitalsStream) Close() error { return nil }

func (g *vitalsGuest) Vitals(_ context.Context, interval time.Duration) (VitalsStream, error) {
	g.gotInterval = interval
	if g.openErr != nil {
		return nil, g.openErr
	}
	return &fakeVitalsStream{samples: g.samples}, nil
}

// newVitalsTestServer builds a Service wired with the vitalsGuest and returns
// the Connect client.
func newVitalsTestServer(t *testing.T, g *vitalsGuest) sandboxv1connect.SandboxClient {
	t.Helper()
	svc := &Service{Guest: func(string) (GuestConn, error) { return g, nil }}
	client, _ := newTestServer(t, svc)
	return client
}

// TestVitalsForwardsTwoSamples is the Task 2.5 acceptance test: the fake guest
// emits two GuestVitals samples and the Service forwards both intact.
func TestVitalsForwardsTwoSamples(t *testing.T) {
	g := &vitalsGuest{
		samples: []*sandboxv1.GuestVitals{
			{SampledAtUnix: 1000, CpuPercent: 10.5, MemUsedBytes: 512 * 1024 * 1024},
			{SampledAtUnix: 1001, CpuPercent: 20.0, MemUsedBytes: 600 * 1024 * 1024},
		},
	}
	client := newVitalsTestServer(t, g)
	ctx := context.Background()

	vstream, err := client.Vitals(ctx, connect.NewRequest(&sandboxv1.VitalsRequest{
		IntervalSeconds: 2,
	}))
	if err != nil {
		t.Fatalf("vitals: %v", err)
	}
	defer vstream.Close()

	var got []*sandboxv1.GuestVitals
	for vstream.Receive() {
		got = append(got, vstream.Msg())
	}
	if serr := vstream.Err(); serr != nil {
		t.Fatalf("stream error: %v", serr)
	}

	if len(got) != 2 {
		t.Fatalf("got %d samples, want 2", len(got))
	}
	if got[0].GetSampledAtUnix() != 1000 || got[0].GetCpuPercent() != 10.5 {
		t.Fatalf("sample[0] = %+v, want sampled_at=1000 cpu=10.5", got[0])
	}
	if got[1].GetSampledAtUnix() != 1001 || got[1].GetCpuPercent() != 20.0 {
		t.Fatalf("sample[1] = %+v, want sampled_at=1001 cpu=20.0", got[1])
	}

	// Assert the interval was forwarded: 2 seconds.
	if g.gotInterval != 2*time.Second {
		t.Fatalf("gotInterval = %v, want 2s", g.gotInterval)
	}
}

// TestVitalsGuestNilReturnsFollowup verifies that a Service without a Guest
// returns the honest #24 follow-up error for Vitals.
func TestVitalsGuestNilReturnsFollowup(t *testing.T) {
	svc := &Service{}
	client, _ := newTestServer(t, svc)
	ctx := context.Background()

	vstream, err := client.Vitals(ctx, connect.NewRequest(&sandboxv1.VitalsRequest{}))
	if err != nil {
		// A server-stream RPC may return the error either on the initial call
		// or on the first Receive. Handle both cases.
		var connErr *connect.Error
		if !errors.As(err, &connErr) {
			t.Fatalf("expected connect.Error, got %T: %v", err, err)
		}
		if connErr.Code() != connect.CodeUnimplemented {
			t.Fatalf("code = %v, want CodeUnimplemented", connErr.Code())
		}
		return
	}
	defer vstream.Close()

	vstream.Receive()
	if serr := vstream.Err(); serr == nil {
		t.Fatal("expected error from nil Guest")
	} else if connect.CodeOf(serr) != connect.CodeUnimplemented {
		t.Fatalf("code = %v, want CodeUnimplemented", connect.CodeOf(serr))
	}
}
