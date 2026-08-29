package provider

import "time"

// The three simulated providers. Each one exists to make a different tradeoff
// visible, because a gateway that only ever chooses between equivalent
// upstreams is not making a decision worth watching.
//
// Prices are in the same shape and rough magnitude as published frontier and
// commodity model pricing, without being any particular vendor's numbers. They
// are here so the cost panel moves for the right reason, not so anyone can
// budget from them.

// Apex is fast, expensive, and reliable: the provider you would pick if money
// were no object, and the one whose failure the demo is built around.
func Apex(seed uint64) *Sim {
	return New(Config{
		Name: "apex",
		Rates: Rates{
			PromptUSDPerMTok:     3.00,
			CompletionUSDPerMTok: 15.00,
		},
		TTFT:           Latency{Median: 90 * time.Millisecond, Sigma: 0.35, Floor: 20 * time.Millisecond},
		PerToken:       Latency{Median: 7 * time.Millisecond, Sigma: 0.30, Floor: 1 * time.Millisecond},
		ErrorRate:      0.002,
		MinReplyTokens: 60,
		MaxReplyTokens: 260,
		Seed:           seed,
	})
}

// Bargain is cheap, slow, and rate-limited: capacity you can afford in bulk but
// cannot lean on all at once. Its token bucket is what makes a failover to it
// interesting rather than free.
func Bargain(seed uint64) *Sim {
	return New(Config{
		Name: "bargain",
		Rates: Rates{
			PromptUSDPerMTok:     0.25,
			CompletionUSDPerMTok: 1.25,
		},
		TTFT:     Latency{Median: 520 * time.Millisecond, Sigma: 0.55, Floor: 80 * time.Millisecond},
		PerToken: Latency{Median: 22 * time.Millisecond, Sigma: 0.40, Floor: 3 * time.Millisecond},
		// Higher than apex because cheap capacity is usually oversubscribed
		// capacity, and the difference should be visible without any injection.
		ErrorRate: 0.010,
		Limit: &RateLimit{
			RequestsPerSecond: 14,
			Burst:             25,
		},
		MinReplyTokens: 60,
		MaxReplyTokens: 260,
		Seed:           seed + 1,
	})
}

// Local is free and capacity-constrained: a fixed-size box rather than a
// service. It costs nothing per token and refuses outright once its slots are
// full, which is exactly the shape of self-hosted inference.
func Local(seed uint64) *Sim {
	return New(Config{
		Name:           "local",
		Rates:          Rates{},
		TTFT:           Latency{Median: 210 * time.Millisecond, Sigma: 0.45, Floor: 40 * time.Millisecond},
		PerToken:       Latency{Median: 17 * time.Millisecond, Sigma: 0.35, Floor: 2 * time.Millisecond},
		ErrorRate:      0.004,
		MaxConcurrent:  6,
		MinReplyTokens: 60,
		MaxReplyTokens: 260,
		Seed:           seed + 2,
	})
}
