package breaker

import (
	"testing"
	"time"
)

// fakeClock drives the breaker without sleeping, so these tests assert on the
// state machine rather than on how fast the test machine happens to be.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

// testConfig is DefaultConfig with time and admission made deterministic.
// admit is a pointer the test moves to choose which side of the ramp a given
// Allow lands on.
func testConfig(clk *fakeClock, roll *float64) Config {
	cfg := DefaultConfig()
	cfg.Now = clk.now
	cfg.Rand = func() float64 { return *roll }
	return cfg
}

func newTestBreaker(t *testing.T) (*Breaker, *fakeClock, *float64) {
	t.Helper()
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	roll := 0.0
	return New(testConfig(clk, &roll)), clk, &roll
}

func record(b *Breaker, successes, failures int) {
	for range successes {
		b.Success()
	}
	for range failures {
		b.Failure()
	}
}

func TestStartsClosed(t *testing.T) {
	b, _, _ := newTestBreaker(t)
	if got := b.State(); got != Closed {
		t.Fatalf("new breaker state = %v, want %v", got, Closed)
	}
	if !b.Allow() {
		t.Fatal("new breaker refused a request")
	}
	if got := b.AdmitRatio(); got != 1 {
		t.Fatalf("new breaker admit ratio = %v, want 1", got)
	}
}

func TestBelowMinRequestsDoesNotTrip(t *testing.T) {
	b, _, _ := newTestBreaker(t)
	// Every request failing, but not enough of them to be evidence. Tripping
	// here would mean a breaker that opens on the first two failures after a
	// deploy, before any traffic has arrived.
	record(b, 0, DefaultConfig().MinRequests-1)

	if got := b.State(); got != Closed {
		t.Fatalf("state after %d failures = %v, want %v (below MinRequests)",
			DefaultConfig().MinRequests-1, got, Closed)
	}
}

func TestTripsWhenFailureRatioReachesThreshold(t *testing.T) {
	b, _, _ := newTestBreaker(t)
	// 5 of 10 failed, threshold is 0.5.
	record(b, 5, 5)

	if got := b.State(); got != Open {
		t.Fatalf("state = %v, want %v", got, Open)
	}
	if b.Allow() {
		t.Fatal("open breaker allowed a request")
	}
	if got := b.AdmitRatio(); got != 0 {
		t.Fatalf("open breaker admit ratio = %v, want 0", got)
	}
	if got := b.Stats().Trips; got != 1 {
		t.Fatalf("trips = %d, want 1", got)
	}
}

func TestStaysClosedBelowThreshold(t *testing.T) {
	b, _, _ := newTestBreaker(t)
	// 4 of 20 failed: a 20% error rate is bad, but it is not an outage, and a
	// breaker that opens here removes 80% of working capacity.
	record(b, 16, 4)

	if got := b.State(); got != Closed {
		t.Fatalf("state = %v, want %v", got, Closed)
	}
}

func TestFailuresOutsideWindowAreForgotten(t *testing.T) {
	b, clk, _ := newTestBreaker(t)
	cfg := DefaultConfig()

	record(b, 0, cfg.MinRequests-1)
	// Walk past the window so those failures age out.
	clk.add(cfg.Window + time.Second)
	record(b, 0, cfg.MinRequests-1)

	if got := b.State(); got != Closed {
		t.Fatalf("state = %v, want %v: failures either side of the window "+
			"should not sum into a trip", got, Closed)
	}
}

func TestOpenBecomesRecoveringAfterCooldown(t *testing.T) {
	b, clk, _ := newTestBreaker(t)
	cfg := DefaultConfig()

	record(b, 5, 5)
	if b.State() != Open {
		t.Fatalf("setup: breaker did not open")
	}

	clk.add(cfg.Cooldown - time.Millisecond)
	if got := b.State(); got != Open {
		t.Fatalf("state just before cooldown expiry = %v, want %v", got, Open)
	}

	clk.add(2 * time.Millisecond)
	if got := b.State(); got != Recovering {
		t.Fatalf("state after cooldown = %v, want %v", got, Recovering)
	}
	if got := b.AdmitRatio(); got != cfg.InitialAdmit {
		t.Fatalf("admit ratio entering recovery = %v, want %v", got, cfg.InitialAdmit)
	}
}

// TestRecoveryIsGradual is the anti-stampede property, and the reason this
// package exists rather than a stock breaker. A textbook half-open state goes
// from admitting nothing to admitting everything in one step; that step is what
// knocks a just-recovered provider back over. Here the admitted fraction has to
// climb through the middle.
func TestRecoveryIsGradual(t *testing.T) {
	b, clk, _ := newTestBreaker(t)
	cfg := DefaultConfig()

	record(b, 5, 5)
	clk.add(cfg.Cooldown)

	var seen []float64
	for range 20 {
		r := b.AdmitRatio()
		seen = append(seen, r)
		if b.State() == Closed {
			break
		}
		clk.add(cfg.RampInterval)
		// Keep the provider looking healthy so nothing re-opens it.
		record(b, 2, 0)
	}

	if len(seen) < 4 {
		t.Fatalf("recovery reached full admission in %d steps; that is a cliff, not a ramp: %v",
			len(seen), seen)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] < seen[i-1] {
			t.Fatalf("admit ratio went backwards during an uninterrupted recovery: %v", seen)
		}
	}
	// The whole point: there is a real interval where some traffic flows and
	// most does not.
	var partial int
	for _, r := range seen {
		if r > 0 && r < 1 {
			partial++
		}
	}
	if partial < 3 {
		t.Fatalf("only %d partial-admission steps in %v; recovery is not gradual", partial, seen)
	}
	if got := seen[len(seen)-1]; got != 1 {
		t.Fatalf("recovery ended at admit ratio %v, want 1", got)
	}
	if got := b.State(); got != Closed {
		t.Fatalf("state after full ramp = %v, want %v", got, Closed)
	}
	if got := b.Stats().Recoveries; got != 1 {
		t.Fatalf("recoveries = %d, want 1", got)
	}
}

// TestRecoveringAdmitsOnlyItsFraction checks that the ramp is actually applied
// to admission and not merely reported.
func TestRecoveringAdmitsOnlyItsFraction(t *testing.T) {
	b, clk, roll := newTestBreaker(t)
	cfg := DefaultConfig()

	record(b, 5, 5)
	clk.add(cfg.Cooldown)
	if b.State() != Recovering {
		t.Fatalf("setup: state = %v, want %v", b.State(), Recovering)
	}

	// A roll below the admitted fraction gets in; one above does not.
	*roll = cfg.InitialAdmit / 2
	if !b.Allow() {
		t.Fatalf("roll %v below admit ratio %v was refused", *roll, cfg.InitialAdmit)
	}
	*roll = cfg.InitialAdmit + 0.01
	if b.Allow() {
		t.Fatalf("roll %v above admit ratio %v was admitted", *roll, cfg.InitialAdmit)
	}
}

func TestFailureDuringRecoveryReopensWithLongerCooldown(t *testing.T) {
	b, clk, _ := newTestBreaker(t)
	cfg := DefaultConfig()

	record(b, 5, 5)
	clk.add(cfg.Cooldown)
	if b.State() != Recovering {
		t.Fatalf("setup: state = %v, want %v", b.State(), Recovering)
	}

	// The provider is still broken.
	record(b, 0, cfg.MinRequests)
	if got := b.State(); got != Open {
		t.Fatalf("state after failing recovery = %v, want %v", got, Open)
	}

	// The first cooldown has now been served once, so retrying at the same
	// cadence would hammer a provider that has already proved it is not ready.
	clk.add(cfg.Cooldown + time.Second)
	if got := b.State(); got != Open {
		t.Fatalf("state = %v, want %v: cooldown should have backed off past %v",
			got, Open, cfg.Cooldown)
	}

	clk.add(cfg.Cooldown * time.Duration(cfg.BackoffFactor))
	if got := b.State(); got != Recovering {
		t.Fatalf("state after backed-off cooldown = %v, want %v", got, Recovering)
	}
}

func TestBackoffIsCapped(t *testing.T) {
	b, clk, _ := newTestBreaker(t)
	cfg := DefaultConfig()

	record(b, 5, 5)
	// Fail a long run of recovery attempts so the exponential would overshoot.
	for range 12 {
		clk.add(cfg.MaxCooldown)
		if b.State() != Recovering {
			t.Fatalf("expected a recovery attempt to be due")
		}
		record(b, 0, cfg.MinRequests)
	}

	clk.add(cfg.MaxCooldown + time.Second)
	if got := b.State(); got != Recovering {
		t.Fatalf("state = %v, want %v: cooldown grew past MaxCooldown", got, Recovering)
	}
}

// TestProbeEarlyRecoveryIsOffByDefault pins the conservative default. A probe
// is a cheap, shallow request; a dependency can answer one correctly while
// failing every real request it is given, so probes alone must not be enough to
// collapse a backoff.
func TestProbeEarlyRecoveryIsOffByDefault(t *testing.T) {
	b, clk, _ := newTestBreaker(t)
	cfg := DefaultConfig()

	if cfg.ProbeEarlyRecovery {
		t.Fatal("DefaultConfig enables probe-driven early recovery; it must be opt-in")
	}
	if b.ProbeEarlyRecovery() {
		t.Fatal("a default breaker has probe-driven early recovery on")
	}

	record(b, 5, 5)
	if b.State() != Open {
		t.Fatalf("setup: breaker did not open")
	}

	// Well inside the cooldown, with far more passing probes than the enabled
	// path would need.
	clk.add(cfg.Cooldown / 4)
	for range cfg.ProbeSuccessesToRecover * 5 {
		b.Success()
	}

	if got := b.State(); got != Open {
		t.Fatalf("state = %v, want %v: probes must not shorten the cooldown "+
			"unless early recovery is enabled", got, Open)
	}

	// The cooldown still expires normally; the flag changes when recovery
	// starts, not whether it ever does.
	clk.add(cfg.Cooldown)
	if got := b.State(); got != Recovering {
		t.Fatalf("state after the full cooldown = %v, want %v", got, Recovering)
	}
}

// TestProbeSuccessCutsCooldownShort covers the affordance the demo enables: the
// backoff was sized by an outage that has now ended, and the health probe is the
// only thing that can say so, because an open breaker sends no traffic.
func TestProbeSuccessCutsCooldownShort(t *testing.T) {
	b, clk, _ := newTestBreaker(t)
	cfg := DefaultConfig()
	b.SetProbeEarlyRecovery(true)

	record(b, 5, 5)
	if b.State() != Open {
		t.Fatalf("setup: breaker did not open")
	}

	// Well inside the cooldown, so time alone would not move it.
	clk.add(cfg.Cooldown / 4)
	for range cfg.ProbeSuccessesToRecover {
		b.Success()
	}

	if got := b.State(); got != Recovering {
		t.Fatalf("state after %d probe successes = %v, want %v",
			cfg.ProbeSuccessesToRecover, got, Recovering)
	}
	if got := b.AdmitRatio(); got != cfg.InitialAdmit {
		t.Fatalf("admit ratio = %v, want %v: an early recovery must still ramp",
			got, cfg.InitialAdmit)
	}
}

func TestProbeSuccessesMustBeConsecutive(t *testing.T) {
	b, clk, _ := newTestBreaker(t)
	cfg := DefaultConfig()
	b.SetProbeEarlyRecovery(true)

	record(b, 5, 5)
	clk.add(cfg.Cooldown / 4)

	// A flapping provider: success, failure, success. Never two in a row.
	for range 5 {
		b.Success()
		b.Failure()
	}

	if got := b.State(); got != Open {
		t.Fatalf("state = %v, want %v: a flapping provider should not "+
			"restart the ramp on every good probe", got, Open)
	}
}

// TestProbeEarlyRecoveryTogglesAtRuntime covers the path the admin endpoint
// uses: the demo turns this on mid-incident, and restarting to pick up a flag
// would destroy the state being demonstrated.
func TestProbeEarlyRecoveryTogglesAtRuntime(t *testing.T) {
	b, clk, _ := newTestBreaker(t)
	cfg := DefaultConfig()

	record(b, 5, 5)
	clk.add(cfg.Cooldown / 4)

	for range cfg.ProbeSuccessesToRecover {
		b.Success()
	}
	if got := b.State(); got != Open {
		t.Fatalf("state with the flag off = %v, want %v", got, Open)
	}

	b.SetProbeEarlyRecovery(true)
	for range cfg.ProbeSuccessesToRecover {
		b.Success()
	}
	if got := b.State(); got != Recovering {
		t.Fatalf("state after enabling the flag = %v, want %v", got, Recovering)
	}
	if got := b.Stats().ProbeEarlyRecovery; !got {
		t.Fatal("Stats does not report that early recovery is enabled")
	}
}

func TestStatsReportWindowCounts(t *testing.T) {
	b, _, _ := newTestBreaker(t)
	record(b, 3, 2)

	got := b.Stats()
	if got.Successes != 3 || got.Failures != 2 {
		t.Fatalf("stats = %+v, want 3 successes and 2 failures", got)
	}
	if got.State != "closed" {
		t.Fatalf("stats state = %q, want %q", got.State, "closed")
	}
}

func TestZeroConfigGetsDefaults(t *testing.T) {
	b := New(Config{})
	if !b.Allow() {
		t.Fatal("zero-config breaker refused a request")
	}
	record(b, 5, 5)
	if got := b.State(); got != Open {
		t.Fatalf("zero-config breaker state = %v, want %v: defaults were not applied", got, Open)
	}
}

func TestStateString(t *testing.T) {
	for state, want := range map[State]string{
		Closed:     "closed",
		Open:       "open",
		Recovering: "recovering",
		State(99):  "unknown",
	} {
		if got := state.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", state, got, want)
		}
	}
}
