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
	"context"
	"fmt"
	"time"

	"github.com/attestantio/dirk/services/headtracker"
	"github.com/attestantio/go-eth2-client/spec/phase0"
)

const (
	opAttest   = "attestation"
	opProposal = "proposal"

	reasonApproved   = "approved"
	reasonStale      = "denied_stale"
	reasonBlockRoot  = "denied_block_root"
	reasonCheckpoint = "denied_checkpoint"
	reasonSlot       = "denied_slot"
	reasonParentRoot = "denied_parent_root"
)

// CheckAttestation verifies that the supplied attestation data is consistent
// with the beacon node's view of the chain.  Returns nil on approval, or a
// non-nil error describing the denial reason.
func (s *Service) CheckAttestation(ctx context.Context, data *headtracker.AttestationData) error {
	// Load the latest snapshot.
	st := s.state.Load()

	// Check if it's stale.
	if age := time.Since(st.lastUpdate); age > s.stalenessThreshold {
		s.monitor.HeadTrackerCheck(opAttest, reasonStale)

		return fmt.Errorf("local beacon view is stale (%s since last head update, threshold %s)", age, s.stalenessThreshold)
	}

	// Validate the request's root is in the local head's recent chain.
	//
	// On a late block the local beacon may not have imported this root yet,
	// even though it is a valid head the node is about to adopt.  Rather than
	// denying immediately we wait, up to the slot's attestation deadline, for
	// the root to appear on our LOCAL canonical chain.
	//
	// The wait only ever re-reads our own beacon's snapshot: a root is
	// approved solely because the local node adopted it, never because the
	// requester asserted it.  A compromised client cannot induce a signature
	// for a root our node does not itself know about. At worst, it induces a
	// delay that ends in denial.
	st, blockRootKnown := s.awaitBlockRootOnChain(ctx, st, data.BeaconBlockRoot, data.Slot)
	if !blockRootKnown {
		s.monitor.HeadTrackerCheck(opAttest, reasonBlockRoot)

		return fmt.Errorf(
			"attestation block root %#x is not on the local head's recent chain (head %#x at slot %d)",
			data.BeaconBlockRoot, st.root, st.slot,
		)
	}

	justified := st.justified

	// On an epoch boundary our cached justified checkpoint can lag the beacon
	// by one epoch until the next refresh is triggered by a head event or our
	// backup periodic polling, even though the beacon would report the
	// advanced checkpoint if asked.
	//
	// So when the request's source is exactly one epoch ahead, re-query our
	// own beacon's finality, bounded by the slot's attestation deadline,
	// before deciding, rather than denying a checkpoint we would hold had we
	// just refreshed.
	if data.Source.Epoch != justified.Epoch || data.Source.Root != justified.Root {
		justified = s.awaitJustifiedCheckpoint(ctx, justified, data.Source, data.Slot)
	}

	// Check the request's epoch and root match our latest justified checkpoint.
	if data.Source.Epoch != justified.Epoch || data.Source.Root != justified.Root {
		s.monitor.HeadTrackerCheck(opAttest, reasonCheckpoint)

		return fmt.Errorf("attestation source checkpoint %d/%#x does not match local justified checkpoint %d/%#x",
			data.Source.Epoch, data.Source.Root, justified.Epoch, justified.Root)
	}

	// Determine the current epoch.
	currentEpoch := phase0.Epoch(uint64(s.currentSlot()) / s.slotsPerEpoch)

	// Allow target to be the previous, current, or next epoch.
	// If a requester builds the attestation just inside an epoch boundary it
	// might be a slot ahead or behind our own view due to clock drift between
	// machines.
	if data.Target.Epoch+1 < currentEpoch || data.Target.Epoch > currentEpoch+1 {
		s.monitor.HeadTrackerCheck(opAttest, reasonCheckpoint)

		return fmt.Errorf("attestation target epoch %d outside acceptable window around local current epoch %d",
			data.Target.Epoch, currentEpoch)
	}

	// Success!
	s.monitor.HeadTrackerCheck(opAttest, reasonApproved)

	return nil
}

// awaitBlockRootOnChain reports whether root is on the local node's recent
// canonical chain, polling for it to appear if it is not yet present so that a
// block still propagating to the local beacon is not denied prematurely.
//
// The wait is bounded by the slot's attestation deadline, an absolute offset
// from the start of the slot rather than a budget relative to when the request
// arrived, so it never blocks past the point a signature stops being useful. It
// returns at once when the wait is disabled (offset <= 0) or that deadline has
// already passed, and stops early if ctx is cancelled.
//
// Only the service's own snapshot is ever consulted, so root is accepted solely
// because the local node adopted it, never on the requester's say so.
//
// It returns the freshest snapshot it observed and whether root was found.
func (s *Service) awaitBlockRootOnChain(
	ctx context.Context,
	st *snapshot,
	root phase0.Root,
	slot phase0.Slot,
) (*snapshot, bool) {
	// Return early if we already know of this block.
	if _, known := st.chain[root]; known {
		return st, true
	}

	// Otherwise, return early if waiting is disabled.
	if s.attestationDeadlineOffset <= 0 {
		// Wait disabled: preserve immediate denial.
		return st, false
	}

	// Calculate a deadline based on the slot start time and our attestation offset.
	deadline := s.slotStartTime(slot).Add(s.attestationDeadlineOffset)

	if !time.Now().Before(deadline) {
		// Already past the slot's attestation deadline.
		return st, false
	}

	// Create a new context with our deadline.
	waitCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	// Create a new ticker based on the configured polling interval.
	interval := s.blockRootPollInterval
	if interval <= 0 {
		interval = defaultBlockRootPollInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Poll until either the context is cancelled or the block appears.
	for {
		select {
		case <-waitCtx.Done():
			return s.state.Load(), false
		case <-ticker.C:
			st = s.state.Load()
			if _, known := st.chain[root]; known {
				return st, true
			}
		}
	}
}

// awaitJustifiedCheckpoint resolves the epoch-boundary race where our cached
// justified checkpoint still lags the beacon by an epoch.  Across a boundary the
// cached finality only advances on the next refresh (a head event or the
// periodic poll), so an attestation whose source is the freshly justified
// checkpoint can arrive before that refresh and be denied on a checkpoint we
// would hold moments later.
//
// It returns the most recent justified checkpoint observed.
func (s *Service) awaitJustifiedCheckpoint(
	ctx context.Context,
	cached phase0.Checkpoint,
	source phase0.Checkpoint,
	slot phase0.Slot,
) phase0.Checkpoint {
	// Only the boundary race: source exactly one epoch ahead of our cached view.
	if source.Epoch != cached.Epoch+1 {
		return cached
	}

	// Otherwise, return early if waiting is disabled.
	if s.attestationDeadlineOffset <= 0 {
		return cached
	}

	// Bound the re-query by the same slot attestation deadline as the block-root
	// wait, so the two never together exceed the point a signature stops being
	// useful.
	deadline := s.slotStartTime(slot).Add(s.attestationDeadlineOffset)
	if !time.Now().Before(deadline) {
		return cached
	}

	waitCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	interval := s.blockRootPollInterval
	if interval <= 0 {
		interval = defaultBlockRootPollInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	latest := cached
	for {
		// Re-query our own beacon's finality at the current slot, publishing it
		// so a concurrent attestation at the same boundary need not re-query. A
		// response that has crossed the boundary ends the wait immediately.
		if justified, err := s.refreshFinality(waitCtx); err == nil {
			latest = justified
			if latest.Epoch >= source.Epoch {
				return latest
			}
		}

		select {
		case <-waitCtx.Done():
			return latest
		case <-ticker.C:
		}
	}
}

// CheckProposal verifies that the supplied proposal data is consistent with
// the beacon node's view of the chain head.  Returns nil on approval, or a
// non-nil error describing the denial reason.
func (s *Service) CheckProposal(_ context.Context, data *headtracker.ProposalData) error {
	// Load the latest snapshot.
	st := s.state.Load()

	// Check if it's stale.
	if age := time.Since(st.lastUpdate); age > s.stalenessThreshold {
		s.monitor.HeadTrackerCheck(opProposal, reasonStale)

		return fmt.Errorf("local beacon view is stale (%s since last head update, threshold %s)", age, s.stalenessThreshold)
	}

	// Get the current slot.
	currentSlot := s.currentSlot()

	// A proposer typically signs for the current slot, occasionally one ahead
	// if the duty was scheduled and the slot boundary is imminent.  Allow a
	// one-slot window either side to absorb wall-clock jitter.
	if data.Slot+1 < currentSlot || data.Slot > currentSlot+1 {
		s.monitor.HeadTrackerCheck(opProposal, reasonSlot)

		return fmt.Errorf("proposal slot %d outside acceptable window around local current slot %d",
			data.Slot, currentSlot)
	}

	// Check the proposal parent root is in the local head's recent chain.
	if _, parentKnown := st.chain[data.ParentRoot]; !parentKnown {
		s.monitor.HeadTrackerCheck(opProposal, reasonParentRoot)

		return fmt.Errorf("proposal parent root %#x is not on the local head's recent chain (head %#x at slot %d)",
			data.ParentRoot, st.root, st.slot)
	}

	// Success!
	s.monitor.HeadTrackerCheck(opProposal, reasonApproved)

	return nil
}
