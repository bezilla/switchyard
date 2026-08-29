// Package breaker implements a circuit breaker whose recovery is gradual.
//
// The textbook breaker has three states and a sharp edge: closed, open, and a
// half-open state that admits one probe and, if that probe succeeds, closes
// fully. That last transition is a stampede generator. A provider that has been
// out for a minute comes back to a cold cache, cold connection pools and a
// scheduler with nothing warm on it, and the first thing it meets is one
// hundred percent of the traffic it was failing under. It falls over, the
// breaker opens again, and the system oscillates.
//
// This breaker replaces the sharp edge with a ramp. Recovery admits a small
// fraction of traffic, and that fraction grows geometrically while the provider
// keeps succeeding. Failures during the ramp send it back to open with a longer
// cooldown. The provider gets to warm up under load it can survive.
package breaker

import (
	"math"
	"math/rand"
	"sync"
	"time"
)

// State is which of the three states a breaker is in.
type State int

const (
	// Closed passes all traffic. The normal state.
	Closed State = iota

	// Open passes none. The provider is considered broken.
	Open

	// Recovering passes a growing fraction. This is half-open with a ramp
	// instead of a cliff.
	Recovering
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	case Recovering:
		return "recovering"
	default:
		return "unknown"
	}
}

// Config tunes a breaker.
type Config struct {
	// Window is how far back the failure ratio looks.
	Window time.Duration

	// Buckets divides Window for rolling counts. More buckets means the
	// window slides more smoothly and costs slightly more memory.
	Buckets int

	// MinRequests is the sample size below which the failure ratio is not
	// trusted. Two failed requests out of two is not evidence of an outage.
	MinRequests int

	// FailureRatio in [0,1] is the trip threshold.
	FailureRatio float64

	// Cooldown is how long the first Open lasts before a recovery attempt.
	// Repeated trips multiply it by BackoffFactor, capped at MaxCooldown.
	Cooldown      time.Duration
	BackoffFactor float64
	MaxCooldown   time.Duration

	// InitialAdmit is the fraction of traffic admitted at the start of
	// recovery. Small on purpose: this is the trickle that warms the provider.
	InitialAdmit float64

	// RampFactor multiplies the admitted fraction every RampInterval of
	// uninterrupted success. Geometric growth reaches full traffic quickly
	// while still spending real time at low volume.
	RampFactor   float64
	RampInterval time.Duration

	// ProbeEarlyRecovery lets consecutive successes recorded while open cut
	// the cooldown short, instead of waiting the backoff out. It is OFF by
	// default: see the commentary on Breaker.SetProbeEarlyRecovery for the
	// failure mode it introduces.
	ProbeEarlyRecovery bool

	// ProbeSuccessesToRecover is how many consecutive successes recorded
	// while open cut the cooldown short, when ProbeEarlyRecovery is on.
	// Nothing is admitted while open, so the only thing that can produce a
	// success in that state is an out-of-band health probe.
	ProbeSuccessesToRecover int

	// Now and Rand are injected so tests can drive time and admission without
	// sleeping or flaking. Both default sensibly when left nil.
	Now  func() time.Time
	Rand func() float64
}

// DefaultConfig is the configuration Switchyard runs with. The numbers are
// chosen for a demo you watch in real time: a provider that breaks is shunned
// within a couple of seconds and takes roughly fifteen seconds to ramp back to
// full traffic, which is slow enough to see on a dashboard and fast enough to
// hold an audience.
func DefaultConfig() Config {
	return Config{
		Window:        10 * time.Second,
		Buckets:       10,
		MinRequests:   8,
		FailureRatio:  0.5,
		Cooldown:      5 * time.Second,
		BackoffFactor: 2,
		MaxCooldown:   60 * time.Second,
		InitialAdmit:  0.05,
		RampFactor:    1.6,
		RampInterval:  900 * time.Millisecond,

		// Off by default. The demo turns it on to keep `make heal-apex`
		// responsive; a real deployment should decide deliberately.
		ProbeEarlyRecovery: false,

		// Two rather than one: a single probe succeeding against a provider
		// that is flapping would restart the ramp on every flap. Two is a
		// mitigation, not a fix -- a dependency that answers probes but
		// cannot serve traffic passes both of them just as easily as one.
		ProbeSuccessesToRecover: 2,
	}
}

type bucket struct {
	start    time.Time
	success  int
	failure  int
	occupied bool
}

// Breaker is one provider's circuit. It is safe for concurrent use.
type Breaker struct {
	cfg Config

	mu       sync.Mutex
	state    State
	buckets  []bucket
	openedAt time.Time
	cooldown time.Duration

	// recovery ramp
	admit         float64
	lastRamp      time.Time
	recoveredFrom int // consecutive trips, drives the backoff

	// openSuccess counts consecutive successes seen while open, which can
	// only come from health probes.
	openSuccess int

	// earlyRecovery mirrors Config.ProbeEarlyRecovery and can be flipped at
	// runtime, because the demo turns it on for one command and a restart in
	// the middle of a failover would destroy the thing being demonstrated.
	earlyRecovery bool

	// transitions counts state changes, which is what a dashboard graphs.
	trips      int
	recoveries int
}

// New builds a breaker, filling in any unset config with defaults.
func New(cfg Config) *Breaker {
	d := DefaultConfig()
	if cfg.Window <= 0 {
		cfg.Window = d.Window
	}
	if cfg.Buckets <= 0 {
		cfg.Buckets = d.Buckets
	}
	if cfg.MinRequests <= 0 {
		cfg.MinRequests = d.MinRequests
	}
	if cfg.FailureRatio <= 0 {
		cfg.FailureRatio = d.FailureRatio
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = d.Cooldown
	}
	if cfg.BackoffFactor < 1 {
		cfg.BackoffFactor = d.BackoffFactor
	}
	if cfg.MaxCooldown <= 0 {
		cfg.MaxCooldown = d.MaxCooldown
	}
	if cfg.InitialAdmit <= 0 {
		cfg.InitialAdmit = d.InitialAdmit
	}
	if cfg.RampFactor <= 1 {
		cfg.RampFactor = d.RampFactor
	}
	if cfg.RampInterval <= 0 {
		cfg.RampInterval = d.RampInterval
	}
	if cfg.ProbeSuccessesToRecover <= 0 {
		cfg.ProbeSuccessesToRecover = d.ProbeSuccessesToRecover
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Rand == nil {
		cfg.Rand = func() float64 { return rand.Float64() } //nolint:gosec // load shaping, not security
	}
	return &Breaker{
		cfg:           cfg,
		state:         Closed,
		buckets:       make([]bucket, cfg.Buckets),
		cooldown:      cfg.Cooldown,
		admit:         1,
		earlyRecovery: cfg.ProbeEarlyRecovery,
	}
}

// Allow reports whether a request may be sent, and advances any state change
// that has become due. Callers must follow an Allow that returned true with
// exactly one Success or Failure.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.cfg.Now()
	b.advance(now)

	switch b.state {
	case Closed:
		return true
	case Open:
		return false
	case Recovering:
		return b.cfg.Rand() < b.admit
	default:
		return true
	}
}

// advance applies time-driven transitions: open expiring into recovery, and the
// recovery ramp growing. Caller holds the lock.
func (b *Breaker) advance(now time.Time) {
	switch b.state {
	case Open:
		if now.Sub(b.openedAt) >= b.cooldown {
			b.enterRecovering(now)
		}
	case Recovering:
		// Grow the admitted fraction for each whole ramp interval survived.
		for now.Sub(b.lastRamp) >= b.cfg.RampInterval {
			b.lastRamp = b.lastRamp.Add(b.cfg.RampInterval)
			b.admit *= b.cfg.RampFactor
			if b.admit >= 1 {
				b.admit = 1
				b.close(now)
				return
			}
		}
	case Closed:
	}
}

func (b *Breaker) enterRecovering(now time.Time) {
	b.state = Recovering
	b.admit = b.cfg.InitialAdmit
	b.lastRamp = now
	b.openSuccess = 0
	b.resetWindow()
}

func (b *Breaker) close(now time.Time) {
	b.state = Closed
	b.admit = 1
	b.cooldown = b.cfg.Cooldown
	b.recoveredFrom = 0
	b.openSuccess = 0
	b.recoveries++
	b.resetWindow()
	_ = now
}

func (b *Breaker) open(now time.Time) {
	wasRecovering := b.state == Recovering
	b.state = Open
	b.openedAt = now
	b.admit = 0
	b.openSuccess = 0
	b.trips++

	// Tripping straight out of a recovery attempt means the provider is not
	// actually better yet, so wait longer before trying again. A fixed
	// cooldown would retry at the same cadence forever.
	if wasRecovering {
		b.recoveredFrom++
	}
	backoff := float64(b.cfg.Cooldown) * math.Pow(b.cfg.BackoffFactor, float64(b.recoveredFrom))
	if backoff > float64(b.cfg.MaxCooldown) {
		backoff = float64(b.cfg.MaxCooldown)
	}
	b.cooldown = time.Duration(backoff)
	b.resetWindow()
}

// SetProbeEarlyRecovery turns probe-driven early recovery on or off at runtime.
//
// When on, two consecutive health probes succeeding against an open circuit
// start the recovery ramp immediately rather than waiting out the remaining
// cooldown. That is a real convenience -- after repeated trips the backoff
// reaches tens of seconds, and it was sized by an outage that has since ended.
//
// It is off by default because a health probe is a weaker signal than it looks.
// A probe is cheap and shallow; a real request is neither. A dependency whose
// connection pool is exhausted, whose cache is cold, or whose downstream is
// still down can answer a probe correctly while failing every request it is
// given. Two probes passing then collapses a forty-second backoff to about one
// second, the ramp admits traffic, the traffic fails, and the circuit reopens
// with a longer cooldown -- which early recovery will shorten again on the next
// two probes. The result is a flap whose period is set by the probe interval
// rather than by the backoff that exists to damp exactly this.
//
// The backoff is the conservative default for that reason: waiting is cheap
// when there is somewhere else to send the traffic, and a gateway with a
// working failover path always has somewhere else. Enable this when probes are
// known to exercise the same path real requests take.
func (b *Breaker) SetProbeEarlyRecovery(on bool) {
	b.mu.Lock()
	b.earlyRecovery = on
	if !on {
		b.openSuccess = 0
	}
	b.mu.Unlock()
}

// ProbeEarlyRecovery reports whether probe-driven early recovery is enabled.
func (b *Breaker) ProbeEarlyRecovery() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.earlyRecovery
}

// probeEarlyRecovery reads the flag. Caller holds the lock.
func (b *Breaker) probeEarlyRecovery() bool { return b.earlyRecovery }

// Success records a request that worked.
func (b *Breaker) Success() { b.record(true) }

// Failure records a request that did not. Only failures that indicate ill
// health should be reported here: see provider.FailureKind.CountsAgainstHealth.
func (b *Breaker) Failure() { b.record(false) }

func (b *Breaker) record(ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.cfg.Now()
	b.advance(now)

	bk := b.bucketFor(now)
	if ok {
		bk.success++
	} else {
		bk.failure++
	}

	// While open, nothing is admitted, so any success here came from a health
	// probe. When early recovery is enabled, enough of them in a row means the
	// provider is answering again and there is no reason to sit out the rest
	// of a cooldown that was sized by an outage which has ended.
	if b.state == Open {
		if !b.probeEarlyRecovery() {
			// Cooldown only: the backoff is served in full, and probes are
			// recorded for the dashboard without shortening it.
			return
		}
		if ok {
			b.openSuccess++
			if b.openSuccess >= b.cfg.ProbeSuccessesToRecover {
				b.enterRecovering(now)
			}
		} else {
			b.openSuccess = 0
		}
		return
	}

	success, failure := b.totals(now)
	total := success + failure
	if total < b.cfg.MinRequests {
		return
	}
	ratio := float64(failure) / float64(total)

	switch b.state {
	case Closed:
		if ratio >= b.cfg.FailureRatio {
			b.open(now)
		}
	case Recovering:
		// The ramp is a probation. Judging it by the same ratio, rather than
		// by "any failure at all", keeps a provider with a normal baseline
		// error rate from being unable to ever finish recovering.
		if ratio >= b.cfg.FailureRatio {
			b.open(now)
		}
	case Open:
		// Handled above: an open breaker is judged by probes, not by a ratio
		// over traffic it refused to send.
	}
}

// bucketFor returns the rolling bucket covering now, recycling stale ones.
func (b *Breaker) bucketFor(now time.Time) *bucket {
	width := b.cfg.Window / time.Duration(b.cfg.Buckets)
	idx := int(now.UnixNano()/int64(width)) % b.cfg.Buckets
	if idx < 0 {
		idx += b.cfg.Buckets
	}
	bk := &b.buckets[idx]
	start := now.Truncate(width)
	if !bk.occupied || !bk.start.Equal(start) {
		*bk = bucket{start: start, occupied: true}
	}
	return bk
}

// totals sums buckets still inside the window.
func (b *Breaker) totals(now time.Time) (success, failure int) {
	cutoff := now.Add(-b.cfg.Window)
	for i := range b.buckets {
		bk := &b.buckets[i]
		if !bk.occupied || bk.start.Before(cutoff) {
			continue
		}
		success += bk.success
		failure += bk.failure
	}
	return success, failure
}

func (b *Breaker) resetWindow() {
	for i := range b.buckets {
		b.buckets[i] = bucket{}
	}
}

// State reports the current state, advancing any due transitions first so that
// a caller polling for a dashboard sees the same thing a caller asking Allow
// would see.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.advance(b.cfg.Now())
	return b.state
}

// AdmitRatio reports the fraction of traffic currently admitted: 1 when closed,
// 0 when open, and the ramp position while recovering. Graphing this is how the
// gradual recovery becomes visible rather than merely claimed.
func (b *Breaker) AdmitRatio() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.advance(b.cfg.Now())
	switch b.state {
	case Closed:
		return 1
	case Open:
		return 0
	default:
		return b.admit
	}
}

// Stats is a snapshot for metrics and the admin endpoint.
type Stats struct {
	State      string  `json:"state"`
	AdmitRatio float64 `json:"admit_ratio"`
	Successes  int     `json:"window_successes"`
	Failures   int     `json:"window_failures"`
	Trips      int     `json:"trips"`
	Recoveries int     `json:"recoveries"`

	// ProbeEarlyRecovery reports whether probe successes may cut an open
	// circuit's cooldown short. Surfaced because it changes how long a heal
	// takes to show up, and an operator reading a slow recovery should be
	// able to see whether this is why.
	ProbeEarlyRecovery bool `json:"probe_early_recovery"`
}

// Stats snapshots the breaker.
func (b *Breaker) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.cfg.Now()
	b.advance(now)
	s, f := b.totals(now)
	admit := b.admit
	switch b.state {
	case Closed:
		admit = 1
	case Open:
		admit = 0
	case Recovering:
	}
	return Stats{
		State:              b.state.String(),
		AdmitRatio:         admit,
		Successes:          s,
		Failures:           f,
		Trips:              b.trips,
		Recoveries:         b.recoveries,
		ProbeEarlyRecovery: b.earlyRecovery,
	}
}
