package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// LoadUpstreams parses the -upstreams specification.
//
// The value is either inline JSON or "@" followed by a path to a file holding
// the same JSON. The file form exists because the useful configurations are
// several lines long, and a several-line value inside a compose file's
// environment block is a value nobody will edit correctly.
//
// The document is a list of UpstreamConfig. An empty specification is not an
// error: no upstreams configured is the default and the whole point of the
// simulated providers.
func LoadUpstreams(spec string) ([]UpstreamConfig, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}

	var raw = []byte(spec)
	if path, ok := strings.CutPrefix(spec, "@"); ok {
		b, err := os.ReadFile(strings.TrimSpace(path))
		if err != nil {
			return nil, fmt.Errorf("read upstreams file: %w", err)
		}
		raw = b
	}

	raw, err := stripCommentKeys(raw)
	if err != nil {
		return nil, err
	}

	var cfgs []UpstreamConfig
	dec := json.NewDecoder(bytes.NewReader(raw))
	// A typo in a key would otherwise be silently ignored, and the first
	// symptom would be an upstream behaving as if a setting had never been
	// written -- which it had not.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfgs); err != nil {
		return nil, fmt.Errorf("parse upstreams: %w", err)
	}

	seen := make(map[string]bool, len(cfgs))
	for _, c := range cfgs {
		if seen[c.Name] {
			return nil, fmt.Errorf("duplicate upstream name %q", c.Name)
		}
		seen[c.Name] = true
	}
	return cfgs, nil
}

// BuildUpstreams turns parsed config into providers, resolving each API key
// from the environment variable the config names.
//
// reserved holds names already taken by the simulated providers. A collision is
// rejected rather than resolved, because two providers sharing a name would
// merge into one series on every metric and one line in every dashboard, and
// the failure would look like a routing bug.
func BuildUpstreams(cfgs []UpstreamConfig, reserved []string) ([]*Upstream, error) {
	taken := make(map[string]bool, len(reserved))
	for _, n := range reserved {
		taken[n] = true
	}

	out := make([]*Upstream, 0, len(cfgs))
	for _, cfg := range cfgs {
		if taken[cfg.Name] {
			return nil, fmt.Errorf("upstream %q collides with an existing provider name", cfg.Name)
		}
		taken[cfg.Name] = true

		var key string
		if cfg.APIKeyEnv != "" {
			key = os.Getenv(cfg.APIKeyEnv)
			if key == "" {
				return nil, fmt.Errorf("upstream %q: %s is empty", cfg.Name, cfg.APIKeyEnv)
			}
		}
		up, err := NewUpstream(cfg, key)
		if err != nil {
			return nil, err
		}
		out = append(out, up)
	}
	return out, nil
}

// stripCommentKeys drops top-level keys beginning with "//" from each entry.
//
// JSON has no comments, and this file is the one a reader edits to point the
// gateway at their own endpoint: the fields that matter are the ones whose
// consequences are not obvious from their names, and a config template that
// cannot explain itself sends people to the source. The "//" convention is old
// and unambiguous, and stripping the keys here means the strict decode below
// still catches a real typo rather than treating every unknown key as prose.
func stripCommentKeys(raw []byte) ([]byte, error) {
	var docs []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &docs); err != nil {
		// Not the shape we expected. Hand the original back so the strict
		// decode reports the actual problem instead of this one.
		return raw, nil //nolint:nilerr // the caller's decoder produces the better message
	}
	stripped := false
	for _, doc := range docs {
		for k := range doc {
			if strings.HasPrefix(k, "//") {
				delete(doc, k)
				stripped = true
			}
		}
	}
	if !stripped {
		return raw, nil
	}
	out, err := json.Marshal(docs)
	if err != nil {
		return nil, fmt.Errorf("re-encode upstreams: %w", err)
	}
	return out, nil
}
