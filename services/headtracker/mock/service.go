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

// Package mock provides a mock head tracker service for tests.
package mock

import (
	"context"
	"sync"

	"github.com/attestantio/dirk/services/headtracker"
)

// Service is a configurable mock head tracker.  When AttestationErr or
// ProposalErr is non-nil the corresponding Check method returns it; otherwise
// it approves.  Every request is recorded so tests can assert how the caller
// translated its data into the head tracker's view.
type Service struct {
	AttestationErr error
	ProposalErr    error

	// Attestations and Proposals record, in receipt order, every request passed
	// to the corresponding Check method.
	Attestations []*headtracker.AttestationData
	Proposals    []*headtracker.ProposalData

	// Protects the recorded slices, since the bulk signing path consults the
	// tracker from multiple goroutines.
	mu sync.Mutex
}

// New returns a mock that approves all requests.
func New() *Service {
	return &Service{}
}

// CheckAttestation records the request and returns the configured attestation
// error, or nil.
func (s *Service) CheckAttestation(_ context.Context, data *headtracker.AttestationData) error {
	s.mu.Lock()
	s.Attestations = append(s.Attestations, data)
	s.mu.Unlock()

	return s.AttestationErr
}

// CheckProposal records the request and returns the configured proposal error,
// or nil.
func (s *Service) CheckProposal(_ context.Context, data *headtracker.ProposalData) error {
	s.mu.Lock()
	s.Proposals = append(s.Proposals, data)
	s.mu.Unlock()

	return s.ProposalErr
}
