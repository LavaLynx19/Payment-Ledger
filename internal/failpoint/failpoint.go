// Package failpoint crashes the process at named points in the write paths
// (A§5) so Rung 2 can test recovery. Every point is a no-op unless enabled.
//
// The spec comes from the FAILPOINTS env var: a comma-separated list of
// name=probability, e.g. "capture.after_entries=0.001,accept.after_commit".
// A bare name fires on every hit.
package failpoint

import (
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
)

// Names are the crash points A§5 defines.
func Names() []string {
	return []string{
		"accept.before_commit", "accept.after_commit",
		"capture.after_claim", "capture.after_entries", "capture.after_commit",
		"release.before_commit",
		"sweeper.mid_batch", "purge.mid_batch",
		"reversal.before_commit", "reversal.after_commit",
	}
}

// Set is the enabled failpoints. A nil *Set has every point disabled.
type Set struct {
	probs map[string]float64
	crash func(name string)
}

// Parse reads a FAILPOINTS spec. An empty spec returns nil (all disabled).
// Unknown names are an error, so a typo can't silently disable a test.
func Parse(spec string) (*Set, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	s := &Set{probs: map[string]float64{}, crash: exit}
	known := Names()
	for _, item := range strings.Split(spec, ",") {
		name, prob, hasProb := strings.Cut(strings.TrimSpace(item), "=")
		if !slices.Contains(known, name) {
			return nil, fmt.Errorf("failpoint %q is not one of %s", name, strings.Join(known, ", "))
		}
		p := 1.0
		if hasProb {
			var err error
			if p, err = strconv.ParseFloat(prob, 64); err != nil || p <= 0 || p > 1 {
				return nil, fmt.Errorf("failpoint %s: probability %q must be in (0, 1]", name, prob)
			}
		}
		s.probs[name] = p
	}
	return s, nil
}

// WithCrash replaces what happens when a point fires. Tests use it to record
// or panic instead of exiting.
func (s *Set) WithCrash(fn func(name string)) *Set {
	s.crash = fn
	return s
}

// Inject fires the named point with its configured probability.
func (s *Set) Inject(name string) {
	if s == nil {
		return
	}
	p, ok := s.probs[name]
	if !ok {
		return
	}
	if p >= 1 || rand.Float64() < p {
		s.crash(name)
	}
}

// String lists the enabled points for startup logs.
func (s *Set) String() string {
	if s == nil {
		return "none"
	}
	parts := make([]string, 0, len(s.probs))
	for name, p := range s.probs {
		parts = append(parts, fmt.Sprintf("%s=%g", name, p))
	}
	slices.Sort(parts)
	return strings.Join(parts, ",")
}

// exit mimics kill -9. Inside a container the process is PID 1, and the
// kernel ignores a SIGKILL that PID 1 sends itself, so it exits instead:
// defers are skipped, connections drop, and Postgres rolls back the open tx.
func exit(name string) {
	log.Printf("failpoint %s: crashing", name)
	os.Exit(137)
}
