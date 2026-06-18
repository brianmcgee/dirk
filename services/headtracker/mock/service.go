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

	"github.com/attestantio/dirk/services/headtracker"
)

// Service is a configurable mock head tracker.  When AttestationErr or
// ProposalErr is non-nil the corresponding Check method returns it; otherwise
// it approves.
type Service struct {
	AttestationErr error
	ProposalErr    error
}

// New returns a mock that approves all requests.
func New() *Service {
	return &Service{}
}

// CheckAttestation returns the configured attestation error, or nil.
func (s *Service) CheckAttestation(_ context.Context, _ *headtracker.AttestationData) error {
	return s.AttestationErr
}

// CheckProposal returns the configured proposal error, or nil.
func (s *Service) CheckProposal(_ context.Context, _ *headtracker.ProposalData) error {
	return s.ProposalErr
}
