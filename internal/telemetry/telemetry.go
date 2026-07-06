// Package telemetry is a privacy-first PRODUCT-USAGE telemetry pipeline for the
// hosted Mitos binaries. It emits a small, curated set of product-analytics
// events (sandbox.created, sandbox.forked, signup.started, signup.verified)
// through a pluggable Sink so the operators of the hosted offering can measure
// adoption without ever collecting PII.
//
// This is DELIBERATELY distinct from internal/observability (OpenTelemetry
// distributed tracing). That package records spans for request-path debugging;
// this one records discrete product events for analytics. They share neither
// data model nor transport, and conflating them would either leak request-path
// PII into analytics or pollute traces with product events. The construction and
// config style here mirror observability.Setup (off by default, a FromEnv
// constructor, a startup log line, a shutdown func) on purpose.
//
// PRIVACY IS THE PRODUCT. The non-negotiable guarantees:
//
//   - DISABLED by default. Emitting requires an explicit opt-in (Config.Enabled
//     or MITOS_TELEMETRY_ENABLED=true) AND a configured sink endpoint. With no
//     config the constructor returns a no-op emitter whose Emit allocates
//     nothing and touches no sink.
//   - DO_NOT_TRACK is honored unconditionally: if the DO_NOT_TRACK env var is
//     "1" or "true", telemetry is force-disabled regardless of every other
//     setting. An explicit per-deploy opt-out (Config.OptOut) does the same.
//   - NO PII. The org id is sent ONLY as a salted, one-way hash; without a salt
//     configured the id is dropped entirely (fail closed: never send a raw org
//     id or email). Event properties pass a deny-list that drops high-risk keys
//     (email, ip, token, secret, password, and similar) so a caller cannot
//     accidentally attach PII.
//
// The salt and any endpoint credential are SECRETS: they are never logged.
package telemetry

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Event is one product-usage event. Name is a stable analytics identifier (for
// example "sandbox.created"). Properties are non-PII attributes; they are passed
// through the deny-list sanitizer before send. OrgID is the RAW org id supplied
// by the caller: the Emitter hashes it with the configured salt before it ever
// reaches a sink, and drops it when no salt is set. Timestamp is the event time;
// when zero the Emitter stamps it at Emit.
type Event struct {
	Name       string
	Properties map[string]any
	OrgID      string
	Timestamp  time.Time
}

// sentEvent is the shape that actually crosses the Sink boundary. It carries the
// HASHED org id only (OrgHash), never the raw OrgID, so a sink (and the wire) can
// never observe a raw account identifier. The JSON tags are the stable on-wire
// contract documented in docs/telemetry.md.
type sentEvent struct {
	Name       string         `json:"name"`
	Properties map[string]any `json:"properties,omitempty"`
	OrgHash    string         `json:"org_hash,omitempty"`
	Timestamp  time.Time      `json:"timestamp"`
}

// Sink is the transport seam. Send delivers a batch of already-sanitized events.
// It must not block indefinitely; the Emitter calls it from a single background
// goroutine and bounds it with the configured flush timeout. Implementations
// must never re-introduce PII (the events are already sanitized) and must never
// log a credential.
type Sink interface {
	Send(ctx context.Context, events []sentEvent) error
}

// Emitter is the product-telemetry entry point. A disabled Emitter (the default)
// is a cheap no-op: Emit returns immediately with no allocation and no sink
// touch. An enabled Emitter buffers sanitized events on a bounded queue and a
// single background goroutine batches and flushes them to the Sink on a flush
// interval (and on Shutdown). When the queue is full it DROPS the event and
// increments a counter rather than blocking the caller's hot path.
type Emitter struct {
	enabled bool

	// salt is the HMAC key used to hash the org id. Empty means "no salt": the org
	// id is dropped from every event (fail closed). It is a secret; never logged.
	salt []byte

	sink Sink

	queue   chan sentEvent
	dropped atomic.Uint64
	now     func() time.Time

	flushInterval time.Duration
	flushTimeout  time.Duration
	batchMax      int

	wg       sync.WaitGroup
	stopOnce sync.Once
	stop     chan struct{}
}

// noopEmitter is the zero value returned when telemetry is disabled. Its enabled
// field is false, so Emit short-circuits before any allocation.
func noopEmitter() *Emitter { return &Emitter{enabled: false} }

// Enabled reports whether this Emitter will deliver events. A disabled Emitter is
// a guaranteed no-op.
func (e *Emitter) Enabled() bool { return e != nil && e.enabled }

// Dropped returns the number of events dropped because the queue was full. It is
// zero for a disabled emitter.
func (e *Emitter) Dropped() uint64 {
	if e == nil {
		return 0
	}
	return e.dropped.Load()
}

// Emit records one product event. On a disabled Emitter it returns immediately
// with no allocation (the cheap no-op path the privacy rule requires). On an
// enabled Emitter it sanitizes the event (deny-list properties, hash or drop the
// org id), stamps the timestamp if unset, and enqueues it; if the bounded queue
// is full the event is DROPPED and the drop counter is incremented rather than
// blocking the caller.
func (e *Emitter) Emit(ctx context.Context, ev Event) {
	if e == nil || !e.enabled {
		// Cheap no-op: do not allocate, do not sanitize, do not touch the sink.
		return
	}

	se := sentEvent{
		Name:       ev.Name,
		Properties: sanitizeProperties(ev.Properties),
		OrgHash:    e.hashOrg(ev.OrgID),
		Timestamp:  ev.Timestamp,
	}
	if se.Timestamp.IsZero() {
		se.Timestamp = e.now()
	}

	select {
	case e.queue <- se:
	default:
		// Queue full: drop rather than block the caller. The counter lets the
		// operator observe loss without telemetry ever back-pressuring real work.
		e.dropped.Add(1)
	}
	_ = ctx
}

// hashOrg returns the salted one-way hash of a raw org id, or "" when the id is
// empty OR no salt is configured. The empty-salt case is the fail-closed rule:
// rather than send a raw org id we send nothing, so a sink can never recover the
// account identifier. The hash uses HMAC-SHA256 keyed by the salt, so it is not
// reversible to the raw id and not guessable without the salt.
func (e *Emitter) hashOrg(orgID string) string {
	if orgID == "" || len(e.salt) == 0 {
		return ""
	}
	mac := hmac.New(sha256.New, e.salt)
	_, _ = mac.Write([]byte(orgID))
	return hex.EncodeToString(mac.Sum(nil))
}

// run is the single background flush loop. It batches queued events and flushes
// on the flush interval, on a full batch, and on stop. It is the only goroutine
// that ever calls the Sink, so the Sink needs no internal locking.
func (e *Emitter) run() {
	defer e.wg.Done()
	ticker := time.NewTicker(e.flushInterval)
	defer ticker.Stop()

	batch := make([]sentEvent, 0, e.batchMax)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), e.flushTimeout)
		// A sink error is intentionally swallowed: telemetry must never crash or
		// back-pressure the host process. The events are already counted as sent
		// from the caller's perspective; a transport failure drops them silently.
		_ = e.sink.Send(ctx, batch)
		cancel()
		batch = batch[:0]
	}

	for {
		select {
		case se := <-e.queue:
			batch = append(batch, se)
			if len(batch) >= e.batchMax {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-e.stop:
			// Drain whatever is already queued, then do a final flush so Shutdown
			// delivers the buffered events.
			for {
				select {
				case se := <-e.queue:
					batch = append(batch, se)
					if len(batch) >= e.batchMax {
						flush()
					}
					continue
				default:
				}
				break
			}
			flush()
			return
		}
	}
}

// Shutdown stops the background loop and flushes any buffered events. It is safe
// to call on a disabled Emitter (no-op) and safe to call more than once. The
// supplied context bounds how long Shutdown waits for the final flush to finish.
func (e *Emitter) Shutdown(ctx context.Context) error {
	if e == nil || !e.enabled {
		return nil
	}
	e.stopOnce.Do(func() { close(e.stop) })

	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sanitizeProperties returns a copy of props with high-risk keys dropped. The
// deny-list is applied case-insensitively and as a substring match, so "email",
// "user_email", "client_ip", "ip_address", "api_token", "secret_value", and
// "password" are all dropped. This is a guardrail so a caller cannot accidentally
// attach PII to a product event; the documented convention is that callers send
// only counts, names, and tiers. A nil or empty input returns nil so the on-wire
// event omits the properties object.
func sanitizeProperties(props map[string]any) map[string]any {
	if len(props) == 0 {
		return nil
	}
	out := make(map[string]any, len(props))
	for k, v := range props {
		if isHighRiskKey(k) {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// highRiskSubstrings are the case-insensitive substrings that mark a property key
// as PII or secret-bearing and therefore droppable. The list is intentionally
// broad: a false drop loses one analytics dimension, while a false keep can leak
// PII, so the guardrail errs toward dropping.
var highRiskSubstrings = []string{
	"email",
	"ip",
	"token",
	"secret",
	"password",
	"passwd",
	"auth",
	"cookie",
	"address",
	"phone",
	"name", // user names; event NAMES are the Event.Name field, not a property
	"key",  // api keys / secret keys
	"credential",
	"ssn",
}

// isHighRiskKey reports whether a property key matches the deny-list.
func isHighRiskKey(k string) bool {
	lk := strings.ToLower(k)
	for _, s := range highRiskSubstrings {
		if strings.Contains(lk, s) {
			return true
		}
	}
	return false
}
