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

// Package headtracker provides an optional consistency check against a
// (presumably local) beacon node.  When configured, the signer consults the
// head tracker before producing a signature to ensure that the request matches
// the local view of the chain head and the most recent justified / finalized
// checkpoints. When the local view is stale or disagrees the check fails,
// which prevents a compromised coordinator from coercing a malicious signature
// on data the local beacon has not seen.
package headtracker

import (
	"context"

	"github.com/attestantio/go-eth2-client/spec/phase0"
)

// AttestationData is the subset of attestation fields the head tracker needs to
// validate a signing request.
type AttestationData struct {
	Slot            phase0.Slot
	BeaconBlockRoot phase0.Root
	Source          phase0.Checkpoint
	Target          phase0.Checkpoint
}

// ProposalData is the subset of beacon block proposal fields the head tracker
// needs to validate a signing request.
type ProposalData struct {
	Slot       phase0.Slot
	ParentRoot phase0.Root
}

// Service is the interface for the local head consistency check.  An
// implementation returns nil when the request matches the local view, and a
// non-nil error describing the inconsistency otherwise. Errors from this
// service are translated by the signer into a denied result.
type Service interface {
	// CheckAttestation verifies that an attestation is consistent with the
	// local chain head and checkpoints.
	CheckAttestation(ctx context.Context, data *AttestationData) error
	// CheckProposal verifies that a proposal builds on the local head at an
	// appropriate slot.
	CheckProposal(ctx context.Context, data *ProposalData) error
}
