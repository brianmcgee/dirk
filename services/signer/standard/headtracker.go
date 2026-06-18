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
	"github.com/attestantio/dirk/rules"
	"github.com/attestantio/dirk/services/headtracker"
	spec "github.com/attestantio/go-eth2-client/spec/phase0"
)

// headTrackerAttestationData translates a signing request into the subset of
// fields the head tracker checks, converting the request's wire types into the
// spec types the tracker compares against its local beacon view.
func headTrackerAttestationData(data *rules.SignBeaconAttestationData) *headtracker.AttestationData {
	htData := &headtracker.AttestationData{
		Slot:   spec.Slot(data.Slot),
		Source: spec.Checkpoint{Epoch: spec.Epoch(data.Source.Epoch)},
		Target: spec.Checkpoint{Epoch: spec.Epoch(data.Target.Epoch)},
	}
	copy(htData.BeaconBlockRoot[:], data.BeaconBlockRoot)
	copy(htData.Source.Root[:], data.Source.Root)
	copy(htData.Target.Root[:], data.Target.Root)

	return htData
}

// headTrackerProposalData translates a signing request into the subset of
// fields the head tracker checks.
func headTrackerProposalData(data *rules.SignBeaconProposalData) *headtracker.ProposalData {
	htData := &headtracker.ProposalData{
		Slot: spec.Slot(data.Slot),
	}
	copy(htData.ParentRoot[:], data.ParentRoot)

	return htData
}
