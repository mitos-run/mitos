package telemetry

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
)

// Environment variable names. The salt and any endpoint credential are SECRETS
// and are sourced from a secretKeyRef-backed env in the chart; their VALUES are
// never logged.
const (
	// EnvEnabled is the explicit opt-in. Telemetry stays disabled unless this is
	// "1" or "true" AND a sink endpoint is configured.
	EnvEnabled = "MITOS_TELEMETRY_ENABLED"
	// EnvEndpoint is the HTTP collector URL the network sink POSTs to. Empty keeps
	// telemetry disabled even when EnvEnabled is set (fail closed).
	EnvEndpoint = "MITOS_TELEMETRY_ENDPOINT"
	// EnvSalt is the HMAC salt used to hash the org id. SECRET; never logged. With
	// no salt the org id is dropped from every event (fail closed to no id).
	EnvSalt = "MITOS_TELEMETRY_SALT" //nolint:gosec // name of an env var, not a credential
	// EnvToken is an optional bearer token for the collector. SECRET; never logged.
	EnvToken = "MITOS_TELEMETRY_TOKEN" //nolint:gosec // name of an env var, not a credential
	// EnvOptOut is an explicit per-deploy opt-out. "1"/"true" force-disables
	// telemetry regardless of the other settings.
	EnvOptOut = "MITOS_TELEMETRY_OPTOUT"
	// EnvDoNotTrack is the cross-vendor Do Not Track signal. "1"/"true"
	// force-disables telemetry unconditionally, overriding every other setting.
	EnvDoNotTrack = "DO_NOT_TRACK"
)

// Config is the telemetry configuration. The zero value is DISABLED. New(Config)
// returns a no-op emitter unless Enabled is true, a Sink is set, and neither
// OptOut nor DoNotTrack is set.
type Config struct {
	// Enabled is the explicit opt-in. False (the default) means a no-op emitter.
	Enabled bool
	// OptOut is an explicit per-deploy opt-out that force-disables telemetry even
	// when Enabled is true.
	OptOut bool
	// DoNotTrack mirrors the DO_NOT_TRACK signal; true force-disables telemetry
	// unconditionally.
	DoNotTrack bool
	// Salt is the HMAC key used to hash the org id. Empty means the org id is
	// dropped from every event (fail closed). SECRET; never logged.
	Salt string
	// Sink is the transport. When Enabled but Sink is nil, New stays disabled
	// (fail closed: an opt-in with no sink sends nothing).
	Sink Sink
	// SinkName is a non-secret label for the startup log line (for example "http"
	// or "stdout"). It must never be an endpoint or a credential.
	SinkName string

	// FlushInterval is how often the background loop flushes. Default 10s.
	FlushInterval time.Duration
	// FlushTimeout bounds one sink send. Default 5s.
	FlushTimeout time.Duration
	// QueueSize is the bounded queue capacity; a full queue drops with a counter.
	// Default 1024.
	QueueSize int
	// BatchMax is the max events per flush. Default 64.
	BatchMax int

	// Now is the clock, injectable for tests. Default time.Now.
	Now func() time.Time
}

// New builds an Emitter from a Config. It returns a DISABLED no-op emitter (Emit
// is a cheap no-op, no background goroutine) unless ALL of these hold: Config
// .Enabled is true, a Sink is configured, and neither OptOut nor DoNotTrack is
// set. This is the fail-closed contract: when anything is ambiguous or missing,
// telemetry does not run.
//
// A startup log line states enabled/disabled and the sink NAME. It never logs
// the salt, the endpoint, or any credential.
func New(cfg Config, log *slog.Logger) *Emitter {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	disabledReason := ""
	switch {
	case cfg.DoNotTrack:
		disabledReason = "DO_NOT_TRACK is set"
	case cfg.OptOut:
		disabledReason = "explicit opt-out"
	case !cfg.Enabled:
		disabledReason = "not opted in"
	case cfg.Sink == nil:
		disabledReason = "no sink configured"
	}
	if disabledReason != "" {
		log.Info("product telemetry disabled", "reason", disabledReason)
		return noopEmitter()
	}

	e := &Emitter{
		enabled:       true,
		salt:          []byte(cfg.Salt),
		sink:          cfg.Sink,
		now:           cfg.Now,
		flushInterval: cfg.FlushInterval,
		flushTimeout:  cfg.FlushTimeout,
		batchMax:      cfg.BatchMax,
		stop:          make(chan struct{}),
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.flushInterval <= 0 {
		e.flushInterval = 10 * time.Second
	}
	if e.flushTimeout <= 0 {
		e.flushTimeout = 5 * time.Second
	}
	if e.batchMax <= 0 {
		e.batchMax = 64
	}
	queueSize := cfg.QueueSize
	if queueSize <= 0 {
		queueSize = 1024
	}
	e.queue = make(chan sentEvent, queueSize)

	e.wg.Add(1)
	go e.run()

	// The salt presence is logged as a boolean only; the salt value is never
	// logged. The endpoint is represented only by its non-secret SinkName.
	log.Info("product telemetry enabled",
		"sink", cfg.SinkName,
		"org_hashing", cfg.Salt != "",
	)
	if cfg.Salt == "" {
		log.Warn("product telemetry has no salt configured; org ids will be DROPPED from every event (fail closed)")
	}
	return e
}

// FromEnv builds an Emitter from the environment, the natural constructor for the
// hosted binaries (mirrors observability.Setup reading an endpoint from config).
// It returns a DISABLED no-op emitter unless MITOS_TELEMETRY_ENABLED is truthy
// AND MITOS_TELEMETRY_ENDPOINT is set, and ALWAYS disabled when DO_NOT_TRACK or
// MITOS_TELEMETRY_OPTOUT is truthy. The network sink is HTTPSink pointed at the
// endpoint, with an optional bearer token from MITOS_TELEMETRY_TOKEN. The salt
// comes from MITOS_TELEMETRY_SALT; with no salt the org id is dropped.
//
// None of the secret values (salt, token, endpoint) are logged.
func FromEnv(log *slog.Logger) *Emitter {
	cfg := Config{
		Enabled:    truthy(os.Getenv(EnvEnabled)),
		OptOut:     truthy(os.Getenv(EnvOptOut)),
		DoNotTrack: truthy(os.Getenv(EnvDoNotTrack)),
		Salt:       os.Getenv(EnvSalt),
		SinkName:   "http",
	}
	endpoint := strings.TrimSpace(os.Getenv(EnvEndpoint))
	if endpoint != "" {
		cfg.Sink = NewHTTPSink(endpoint, os.Getenv(EnvToken), nil)
	}
	return New(cfg, log)
}

// truthy reports whether an env value opts in. Only "1" and "true"
// (case-insensitive) count, so an empty or unexpected value stays opted out.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true":
		return true
	default:
		return false
	}
}
