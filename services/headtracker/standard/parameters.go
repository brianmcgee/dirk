// Copyright © 2026 Attestant Limited.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package standard

import (
	"time"

	"github.com/attestantio/dirk/services/metrics"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
)

type parameters struct {
	monitor  metrics.HeadTrackerMonitor
	logLevel zerolog.Level

	address        string
	requestTimeout time.Duration

	slotsPerEpoch  uint64
	secondsPerSlot time.Duration

	ancestorTolerance   uint64
	stalenessThreshold  time.Duration
	maxAttestationDelay time.Duration
}

// Parameter is the interface for service parameters.
type Parameter interface {
	apply(p *parameters)
}

type parameterFunc func(*parameters)

func (f parameterFunc) apply(p *parameters) {
	f(p)
}

// WithLogLevel sets the log level for the module.
func WithLogLevel(logLevel zerolog.Level) Parameter {
	return parameterFunc(func(p *parameters) {
		p.logLevel = logLevel
	})
}

// WithMonitor sets the monitor for this module.
func WithMonitor(monitor metrics.HeadTrackerMonitor) Parameter {
	return parameterFunc(func(p *parameters) {
		p.monitor = monitor
	})
}

// WithAddress sets the address of the beacon node.
func WithAddress(address string) Parameter {
	return parameterFunc(func(p *parameters) {
		p.address = address
	})
}

// WithRequestTimeout sets the request timeout for the beacon node client.
func WithRequestTimeout(timeout time.Duration) Parameter {
	return parameterFunc(func(p *parameters) {
		p.requestTimeout = timeout
	})
}

// WithStalenessThreshold sets the maximum time that may elapse between head
// events from the local beacon node before checks fail closed.
func WithStalenessThreshold(threshold time.Duration) Parameter {
	return parameterFunc(func(p *parameters) {
		p.stalenessThreshold = threshold
	})
}

// WithAncestorTolerance sets the number of slots back from the local head that
// will be accepted as a valid attestation block root or proposal parent root.
func WithAncestorTolerance(tolerance uint64) Parameter {
	return parameterFunc(func(p *parameters) {
		p.ancestorTolerance = tolerance
	})
}

// WithMaxAttestationDelay sets how long, measured from the start of the
// slot being attested, the head tracker will wait for a not-yet-imported
// attestation block root to appear on the local beacon's canonical chain
// before denying.  This absorbs the propagation skew of a late block without
// ever trusting the requester.  A root is only ever approved once the local
// node itself adopts it, so the worst a compromised client can induce is a
// delay that ends in the same denial.
//
// The delay is measured from the start of the slot being attested, not from
// when the request arrives, so it is independent of request timing; once the
// wall clock is past it the tracker denies immediately.  The default of 4.5s
// deliberately sits 0.5s past Vouch's 4s (SECONDS_PER_SLOT/3) attestation
// fallback: when Vouch stops waiting for the block and signs at 4s, that
// margin lets a still-propagating root land here so this Dirk approves rather
// than denies.  A value of zero disables the wait and restores immediate
// denial.
func WithMaxAttestationDelay(delay time.Duration) Parameter {
	return parameterFunc(func(p *parameters) {
		p.maxAttestationDelay = delay
	})
}

// WithSecondsPerSlot overrides the chain seconds-per-slot value used for slot
// derivation.  Defaults to 12s if unset.
func WithSecondsPerSlot(secondsPerSlot time.Duration) Parameter {
	return parameterFunc(func(p *parameters) {
		p.secondsPerSlot = secondsPerSlot
	})
}

// WithSlotsPerEpoch overrides the chain slots-per-epoch value used for epoch
// derivation.  Defaults to 32 if unset.
func WithSlotsPerEpoch(slotsPerEpoch uint64) Parameter {
	return parameterFunc(func(p *parameters) {
		p.slotsPerEpoch = slotsPerEpoch
	})
}

func parseAndCheckParameters(params ...Parameter) (*parameters, error) {
	p := parameters{
		logLevel:            zerolog.GlobalLevel(),
		requestTimeout:      10 * time.Second,
		slotsPerEpoch:       32,
		secondsPerSlot:      12 * time.Second,
		ancestorTolerance:   4,
		stalenessThreshold:  24 * time.Second,
		maxAttestationDelay: 4500 * time.Millisecond,
	}

	for _, param := range params {
		if params != nil {
			param.apply(&p)
		}
	}

	if p.address == "" {
		return nil, errors.New("no beacon node address specified")
	}
	if p.requestTimeout <= 0 {
		return nil, errors.New("request timeout must be positive")
	}
	if p.stalenessThreshold <= 0 {
		return nil, errors.New("staleness threshold must be positive")
	}
	if p.ancestorTolerance == 0 {
		return nil, errors.New("ancestor tolerance must be at least 1")
	}
	if p.maxAttestationDelay < 0 {
		return nil, errors.New("max attestation delay must not be negative")
	}
	if p.secondsPerSlot <= 0 {
		return nil, errors.New("seconds per slot must be positive")
	}
	if p.slotsPerEpoch == 0 {
		return nil, errors.New("slots per epoch must be at least 1")
	}

	return &p, nil
}
