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

// Package standard implements an optional beacon-node head consistency
// check against a single HTTP-attached beacon node.
package standard

import (
	"context"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"github.com/attestantio/dirk/services/metrics"
	eth2client "github.com/attestantio/go-eth2-client"
	"github.com/attestantio/go-eth2-client/api"
	apiv1 "github.com/attestantio/go-eth2-client/api/v1"
	httpclient "github.com/attestantio/go-eth2-client/http"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	zerologger "github.com/rs/zerolog/log"
	"golang.org/x/sync/errgroup"
)

const (
	headBlockID = "head"

	// defaultBlockRootPollInterval is how often the bounded block-root wait
	// re-reads the local head snapshot while waiting for a late block to import.
	defaultBlockRootPollInterval = 50 * time.Millisecond
)

// snapshot is an immutable view of the beacon's head and finality.  A single
// atomic Load yields a consistent (head, finality) pair.  Writers update it via
// updateState, which copies the current snapshot, applies its own fields and
// compare-and-swaps, so the head and finality halves can be refreshed
// independently without locking and without losing a concurrent update.
type snapshot struct {
	chain      map[phase0.Root]phase0.Slot
	root       phase0.Root
	slot       phase0.Slot
	lastUpdate time.Time
	justified  phase0.Checkpoint
	finalized  phase0.Checkpoint
}

// Service tracks the local beacon node's head and finality view, and answers
// consistency questions on behalf of the signer.
type Service struct {
	log zerolog.Logger

	monitor metrics.HeadTrackerMonitor

	headersProvider  eth2client.BeaconBlockHeadersProvider
	finalityProvider eth2client.FinalityProvider

	requestTimeout time.Duration

	slotsPerEpoch  uint64
	secondsPerSlot time.Duration

	ancestorTolerance     uint64
	stalenessThreshold    time.Duration
	maxAttestationDelay   time.Duration
	blockRootPollInterval time.Duration

	genesisTime time.Time

	// state is the latest head and finality snapshot, read lock-free via Load and
	// updated by writers via updateState's compare-and-swap.
	state atomic.Pointer[snapshot]
}

// New creates a new head tracker service.  ctx governs both the lifetime of
// the beacon event subscription and the background maintenance loop.
func New(ctx context.Context, params ...Parameter) (*Service, error) {
	// Parse params.
	p, err := parseAndCheckParameters(params...)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse and check parameters")
	}

	// Init logger.
	log := zerologger.With().
		Str("service", "headtracker").
		Str("impl", "standard").
		Logger()

	if p.logLevel != log.GetLevel() {
		log = log.Level(p.logLevel)
	}

	// Create a beacon client.
	c, err := httpclient.New(ctx,
		httpclient.WithLogLevel(p.logLevel),
		httpclient.WithAddress(p.address),
		httpclient.WithTimeout(p.requestTimeout),
	)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create beacon node client")
	}

	// Ensure it exposes the necessary services.
	headersProvider, isProvider := c.(eth2client.BeaconBlockHeadersProvider)
	if !isProvider {
		return nil, errors.New("beacon node client does not provide beacon block headers")
	}

	finalityProvider, isProvider := c.(eth2client.FinalityProvider)
	if !isProvider {
		return nil, errors.New("beacon node client does not provide finality")
	}

	eventsProvider, isProvider := c.(eth2client.EventsProvider)
	if !isProvider {
		return nil, errors.New("beacon node client does not provide events")
	}

	genesisProvider, isProvider := c.(eth2client.GenesisProvider)
	if !isProvider {
		return nil, errors.New("beacon node client does not provide genesis")
	}

	s := &Service{
		log:                   log,
		monitor:               p.monitor,
		headersProvider:       headersProvider,
		finalityProvider:      finalityProvider,
		requestTimeout:        p.requestTimeout,
		slotsPerEpoch:         p.slotsPerEpoch,
		secondsPerSlot:        p.secondsPerSlot,
		ancestorTolerance:     p.ancestorTolerance,
		stalenessThreshold:    p.stalenessThreshold,
		maxAttestationDelay:   p.maxAttestationDelay,
		blockRootPollInterval: defaultBlockRootPollInterval,
	}

	// Default monitor to a no op if not set.
	if s.monitor == nil {
		s.monitor = &noopMonitor{}
	}

	// Fetch genesis with a defined timeout.
	bootstrapCtx, cancel := context.WithTimeout(ctx, p.requestTimeout)
	defer cancel()

	genesisResp, err := genesisProvider.Genesis(bootstrapCtx, &api.GenesisOpts{})
	if err != nil {
		return nil, errors.Wrap(err, "failed to fetch genesis")
	}

	s.genesisTime = genesisResp.Data.GenesisTime

	// Bootstrap both views concurrently before serving
	bootstrap, bootstrapCtx := errgroup.WithContext(ctx)

	bootstrap.Go(func() error { return s.refreshHead(bootstrapCtx, headBlockID) })
	bootstrap.Go(func() error {
		_, err := s.refreshFinality(bootstrapCtx)
		return err
	})

	if err = bootstrap.Wait(); err != nil {
		return nil, errors.Wrap(err, "failed to bootstrap state")
	}

	// Log the initial state.
	st := s.state.Load()

	log.Info().
		Str("address", p.address).
		Uint64("head_slot", uint64(st.slot)).
		Uint64("justified_epoch", uint64(st.justified.Epoch)).
		Uint64("finalized_epoch", uint64(st.finalized.Epoch)).
		Msg("Head tracker bootstrapped")

	// Subscribe to head events.
	// We refresh state on each head event.
	// The beacon client handles stream reconnection.
	if err = eventsProvider.Events(ctx, []string{"head"}, func(ev *apiv1.Event) {
		s.handleEvent(ctx, ev)
	}); err != nil {
		return nil, errors.Wrap(err, "failed to subscribe to beacon events")
	}

	// Periodically refresh state in the background, in case we miss an SSE event.
	go s.pollPeriodically(ctx)

	return s, nil
}

// refreshHead rebuilds and publishes our view of the chain head and its recent
// ancestors, starting from the provided blockID.
//
// When the head directly extends our previous head the prior window is reused
// and no ancestors are re-fetched.  Otherwise, the head is published first, so
// our bounded block-root wait can act on it immediately, and the ancestry is
// then walked and published.
//
// If the head fetch fails the prior view is left untouched.  The ancestor walk,
// by contrast, is best-effort and tolerates a truncated view due to header
// request failures.
func (s *Service) refreshHead(ctx context.Context, blockID string) error {
	if blockID == "" {
		blockID = headBlockID
	}

	head, err := s.fetchHeader(ctx, blockID)
	if err != nil {
		return errors.Wrap(err, "failed to fetch head")
	}

	prev := s.state.Load()
	headMsg := head.Header.Message

	// Fast path: the new head is a direct descendent of the previous head.
	// The previous set of ancestors are still canonical, so reuse them and
	// skip fetching them again.
	if prev != nil && headMsg.ParentRoot == prev.root {
		chain := make(map[phase0.Root]phase0.Slot, s.ancestorTolerance+1)
		chain[head.Root] = headMsg.Slot
		retainRecentAncestors(chain, prev.chain, s.ancestorTolerance)

		s.publishHead(head.Root, headMsg.Slot, chain)

		return nil
	}

	// Slow path (reorg, gap or bootstrap): publish the new head, then walk and
	// republish as each ancestor is fetched.
	s.publishHead(head.Root, headMsg.Slot,
		map[phase0.Root]phase0.Slot{head.Root: headMsg.Slot})

	// Build the authoritative view of recent blocks, root -> slot, starting with
	// the latest head.
	chain := make(map[phase0.Root]phase0.Slot, s.ancestorTolerance+1)
	chain[head.Root] = headMsg.Slot

	// Iterate through the ancestors until we fetch the configured tolerance.
	currentSlot := headMsg.Slot
	currentRoot := headMsg.ParentRoot

	for i := uint64(0); i < s.ancestorTolerance && currentSlot > 0; i++ {
		ancestor, err := s.fetchHeader(ctx, fmt.Sprintf("%#x", currentRoot[:]))
		if err != nil {
			s.log.Debug().Err(err).
				Str("root", fmt.Sprintf("%#x", currentRoot[:])).
				Msg("Failed to fetch ancestor; truncating chain")

			break
		}
		ancestorMsg := ancestor.Header.Message

		chain[ancestor.Root] = ancestorMsg.Slot

		currentSlot = ancestorMsg.Slot
		currentRoot = ancestorMsg.ParentRoot

		// Publish a fresh view as we fetch each ancestor.
		s.publishHead(head.Root, headMsg.Slot, chain)
	}

	return nil
}

// refreshFinality re-queries the local beacon's finality and merges it into the
// snapshot, leaving the head untouched.  It is the minimal refresh the
// checkpoint check needs at an epoch boundary, where only the justified
// checkpoint moves.  It returns the justified checkpoint observed.
func (s *Service) refreshFinality(ctx context.Context) (phase0.Checkpoint, error) {
	justified, finalized, err := s.fetchFinality(ctx)
	if err != nil {
		return phase0.Checkpoint{}, errors.Wrap(err, "failed to fetch finality")
	}

	s.updateState(func(next *snapshot) {
		next.justified = justified
		next.finalized = finalized
	})

	return justified, nil
}

// retainRecentAncestors copies the tolerance most recent entries with the
// highest slots from src into dst.  It's used when a new head directly
// extends our previous head.
func retainRecentAncestors(dst, src map[phase0.Root]phase0.Slot, tolerance uint64) {
	if tolerance == 0 || len(src) == 0 {
		return
	}

	type entry struct {
		root phase0.Root
		slot phase0.Slot
	}

	entries := make([]entry, 0, len(src))
	for root, slot := range src {
		entries = append(entries, entry{root: root, slot: slot})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].slot > entries[j].slot
	})

	if uint64(len(entries)) > tolerance {
		entries = entries[:tolerance]
	}

	for _, e := range entries {
		dst[e.root] = e.slot
	}
}

// publishHead merges a new head into the snapshot and updates the head-slot
// metric, leaving finality untouched.
func (s *Service) publishHead(
	root phase0.Root,
	slot phase0.Slot,
	chain map[phase0.Root]phase0.Slot,
) {
	s.updateState(func(next *snapshot) {
		next.chain = chain
		next.root = root
		next.slot = slot
		next.lastUpdate = time.Now()
	})

	s.monitor.HeadTrackerHeadSlot(uint64(slot))
}

// updateState applies mutate to a copy of the current snapshot and installs it
// with a compare-and-swap, retrying if a concurrent writer updated the snapshot
// in between.  Each writer touches only its own fields, so the retry re-merges
// onto the latest snapshot rather than clobbering the other half.  The fetches
// run before this; only the in-memory merge is retried, so the loop is cheap.
//
// The copy is shallow: the chain map is shared, never mutated in place.  Head
// writers always install a fresh map and finality writers never touch it.
func (s *Service) updateState(mutate func(next *snapshot)) {
	for {
		old := s.state.Load()

		var next snapshot
		if old != nil {
			next = *old
		}

		mutate(&next)

		if s.state.CompareAndSwap(old, &next) {
			return
		}
	}
}

// fetchHeader retrieves a single beacon block header, applying the configured
// per-request requestTimeout.
func (s *Service) fetchHeader(ctx context.Context, blockID string) (*apiv1.BeaconBlockHeader, error) {
	ctx, cancel := context.WithTimeout(ctx, s.requestTimeout)
	defer cancel()

	resp, err := s.headersProvider.BeaconBlockHeader(ctx, &api.BeaconBlockHeaderOpts{Block: blockID})
	if err != nil {
		return nil, err
	}

	return resp.Data, nil
}

// fetchFinality fetches the justified and finalized checkpoints as of the
// current slot, applying the per-request requestTimeout.
//
// It deliberately queries by the current slot rather than the "head" state. An
// attester derives its source checkpoint from the head state advanced to the
// attestation slot, so across an epoch boundary whose first slot is empty or
// late the source is the freshly justified checkpoint of the epoch just
// entered.  The raw "head" state has not yet crossed that epoch transition and
// reports a justified checkpoint one epoch behind.
//
// Querying by the current slot ensures the cached justified checkpoint matches
// what a correct attester used.
func (s *Service) fetchFinality(ctx context.Context) (phase0.Checkpoint, phase0.Checkpoint, error) {
	ctx, cancel := context.WithTimeout(ctx, s.requestTimeout)
	defer cancel()

	stateID := fmt.Sprintf("%d", uint64(s.currentSlot()))

	resp, err := s.finalityProvider.Finality(ctx, &api.FinalityOpts{State: stateID})
	if err != nil {
		return phase0.Checkpoint{}, phase0.Checkpoint{}, err
	}

	return *resp.Data.Justified, *resp.Data.Finalized, nil
}

// handleEvent processes head SSE events from the beacon node, rebuilding the
// cached ancestor chain and refreshing finality.
func (s *Service) handleEvent(ctx context.Context, ev *apiv1.Event) {
	if ev.Topic != "head" {
		s.log.Debug().Str("topic", ev.Topic).Msg("Ignoring unexpected event topic")
		return
	}

	// Extract the head event.
	head, isHead := ev.Data.(*apiv1.HeadEvent)
	if !isHead || head == nil {
		s.log.Debug().Msg("Head event with unexpected payload type")
		return
	}

	blockID := fmt.Sprintf("%#x", head.Block[:])

	s.log.Trace().
		Uint64("slot", uint64(head.Slot)).
		Str("block", blockID).
		Msg("Head event")

	// Record how far into its slot we learned of this head, for SSE-timeliness
	// analysis.  Only events for the current slot are recorded. A catch-up event
	// after a dropped stream is for an old slot, which we ignore.  A late block
	// still arrives within its own slot, so its delay is captured here.
	if head.Slot == s.currentSlot() {
		s.monitor.HeadTrackerHeadEventDelay(time.Since(s.slotStartTime(head.Slot)).Seconds())
	}

	// Note whether this head is the first we have seen in a new epoch before we
	// refresh, since finality can only have moved across an epoch boundary.
	prev := s.state.Load()
	advancedEpoch := prev == nil || s.epochOf(head.Slot) > s.epochOf(prev.slot)

	// Refresh our view of the chain using this block as the latest head.
	if err := s.refreshHead(ctx, blockID); err != nil {
		s.log.Debug().Err(err).Msg("Head refresh after head event failed")
	}

	// Refresh finality only when this head crossed into a new epoch.  This
	// pre-warms the cached checkpoint before the attestation deadline; the
	// periodic poll and the on-demand checkpoint wait cover the late or empty
	// boundary this misses.
	if advancedEpoch {
		if _, err := s.refreshFinality(ctx); err != nil {
			s.log.Debug().Err(err).Msg("Finality refresh after epoch-advancing head event failed")
		}
	}
}

// pollPeriodically periodically refreshes our cached state as backup in case
// we missed some SSE events. It also publishes the head age metric.
func (s *Service) pollPeriodically(ctx context.Context) {
	// Refresh state twice per slot.
	ticker := time.NewTicker(s.secondsPerSlot / 2)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:

			// Head SSE events only fire on new blocks, so on a chain with
			// frequent empty slots (e.g. Hoodi) a healthy beacon's view would
			// otherwise age past the staleness threshold and deny signing on
			// a perfectly valid head. This poll acts as a liveness check.
			if err := s.refreshHead(ctx, headBlockID); err != nil {
				s.log.Debug().Err(err).Msg("Periodic head refresh failed")
			}

			// Refresh finality unconditionally as the backup for a boundary whose
			// head event we missed or that fell in a fully empty epoch.
			if _, err := s.refreshFinality(ctx); err != nil {
				s.log.Debug().Err(err).Msg("Periodic finality refresh failed")
			}

			// Publish the age of the cached head view.
			age := time.Since(s.state.Load().lastUpdate)
			s.monitor.HeadTrackerRefreshAge(age.Seconds())
		}
	}
}

// currentSlot returns the chain slot derived from wall clock and genesis time.
func (s *Service) currentSlot() phase0.Slot {
	elapsed := time.Since(s.genesisTime)
	if elapsed <= 0 {
		return 0
	}

	return phase0.Slot(uint64(elapsed / s.secondsPerSlot))
}

// slotStartTime returns the wall-clock time at which the given slot begins.
func (s *Service) slotStartTime(slot phase0.Slot) time.Time {
	return s.genesisTime.Add(time.Duration(slot) * s.secondsPerSlot)
}

// epochOf returns the epoch containing the given slot.
func (s *Service) epochOf(slot phase0.Slot) phase0.Epoch {
	return phase0.Epoch(uint64(slot) / s.slotsPerEpoch)
}
