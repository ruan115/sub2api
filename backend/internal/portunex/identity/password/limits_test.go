package password

import (
	"errors"
	"testing"
)

// This deliberately small policy is for synthetic tests, not legacy defaults.
func testLimits() Limits {
	return Limits{
		MaxPHCBytes: 512, MaxPasswordBytes: 1024,
		MinSaltBytes: 8, MaxSaltBytes: 64, MinHashBytes: 16, MaxHashBytes: 64,
		MaxMemoryKiB: 1024, MaxTime: 4, MaxParallelism: 4, MaxWorkKiB: 4096, MaxConcurrent: 2,
	}
}

func TestLimitsRequireExplicitPolicy(t *testing.T) {
	if _, err := NewVerifier(Limits{}); !errors.Is(err, ErrLimits) {
		t.Fatalf("zero policy: %v", err)
	}
	cases := map[string]func(*Limits){
		"phc":             func(l *Limits) { l.MaxPHCBytes = 0 },
		"password":        func(l *Limits) { l.MaxPasswordBytes = 0 },
		"salt minimum":    func(l *Limits) { l.MinSaltBytes = 7 },
		"salt maximum":    func(l *Limits) { l.MaxSaltBytes = 7 },
		"salt input cap":  func(l *Limits) { l.MaxSaltBytes = l.MaxPHCBytes + 1 },
		"hash minimum":    func(l *Limits) { l.MinHashBytes = 3 },
		"hash maximum":    func(l *Limits) { l.MaxHashBytes = 15 },
		"hash input cap":  func(l *Limits) { l.MaxHashBytes = l.MaxPHCBytes + 1 },
		"memory":          func(l *Limits) { l.MaxMemoryKiB = 7 },
		"passes":          func(l *Limits) { l.MaxTime = 0 },
		"parallelism":     func(l *Limits) { l.MaxParallelism = 0 },
		"work":            func(l *Limits) { l.MaxWorkKiB = 7 },
		"concurrency":     func(l *Limits) { l.MaxConcurrent = 0 },
		"negative inputs": func(l *Limits) { l.MaxPHCBytes = -1 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			limits := testLimits()
			change(&limits)
			if _, err := NewVerifier(limits); !errors.Is(err, ErrLimits) {
				t.Fatalf("invalid limits: %v", err)
			}
		})
	}
}

func TestVerifierCopiesPolicy(t *testing.T) {
	limits := testLimits()
	v, err := NewVerifier(limits)
	if err != nil {
		t.Fatal(err)
	}
	limits.MaxConcurrent = 100
	limits.MaxWorkKiB = 100
	if v.limits != testLimits() {
		t.Fatal("caller policy change affected verifier")
	}
}
