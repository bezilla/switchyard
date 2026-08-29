package router

import (
	"context"
	"time"

	"github.com/bezilla/switchyard/internal/provider"
)

// HealthChecker probes every target on an interval and feeds the results into
// the targets' circuit breakers.
//
// Probing exists because request outcomes alone are not enough. A breaker that
// learns only from traffic cannot notice that an idle provider has died, and --
// worse for a failover system -- cannot notice that a provider it is avoiding
// has come back. Once the breaker is open the traffic stops, and with it the
// only evidence that would ever close it again. The probe is the out-of-band
// signal that breaks that circularity.
type HealthChecker struct {
	// Interval is how often each target is probed.
	Interval time.Duration

	// Timeout bounds a single probe. A probe that hangs is a failed probe:
	// waiting longer only delays the failover.
	Timeout time.Duration

	router *Router
}

// NewHealthChecker builds a checker with sensible defaults for anything unset.
func NewHealthChecker(r *Router, interval, timeout time.Duration) *HealthChecker {
	if interval <= 0 {
		interval = time.Second
	}
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &HealthChecker{Interval: interval, Timeout: timeout, router: r}
}

// Run probes until the context is canceled. It probes once immediately so that
// a freshly started gateway does not route on no information for a whole
// interval.
func (h *HealthChecker) Run(ctx context.Context) {
	h.probeAll(ctx)

	t := time.NewTicker(h.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.probeAll(ctx)
		}
	}
}

func (h *HealthChecker) probeAll(ctx context.Context) {
	for _, tgt := range h.router.Targets() {
		h.probe(ctx, tgt)
	}
}

func (h *HealthChecker) probe(ctx context.Context, tgt *Target) {
	pctx, cancel := context.WithTimeout(ctx, h.Timeout)
	defer cancel()

	err := tgt.Provider.Probe(pctx)
	tgt.setProbe(err, time.Now())

	if err == nil {
		tgt.Breaker.Success()
		return
	}
	// Same rule as for real requests: a provider shedding load is telling us
	// something true about our request rate, not about its health.
	if provider.KindOf(err).CountsAgainstHealth() {
		tgt.Breaker.Failure()
	}
}
