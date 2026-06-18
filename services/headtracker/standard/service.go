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

// snapshot is an immutable view of the beacon's head and finality.  It is
// updated atomically via an atomic pointer, so a single Load always yields a
// consistent head and finality pair.
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

	ancestorTolerance         uint64
	stalenessThreshold        time.Duration
	attestationDeadlineOffset time.Duration
	blockRootPollInterval     time.Duration

	genesisTime time.Time

	// state is the latest head and finality snapshot.  Every writer publishes a
	// complete snapshot in a single atomic Store.
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
		log:                       log,
		monitor:                   p.monitor,
		headersProvider:           headersProvider,
		finalityProvider:          finalityProvider,
		requestTimeout:            p.requestTimeout,
		slotsPerEpoch:             p.slotsPerEpoch,
		secondsPerSlot:            p.secondsPerSlot,
		ancestorTolerance:         p.ancestorTolerance,
		stalenessThreshold:        p.stalenessThreshold,
		attestationDeadlineOffset: p.attestationDeadlineOffset,
		blockRootPollInterval:     defaultBlockRootPollInterval,
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

	// Get initial state.
	if err = s.refreshState(ctx, headBlockID); err != nil {
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

// refreshState rebuilds and updates our view of the chain, starting from the
// provided blockID. It fetches finality and the block header concurrently and
// then publishes a recent view of the chain.
//
// When the head directly extends our previous head the prior window is reused
// and no ancestors are re-fetched.  Otherwise, the head is published first and
// the ancestry is then walked from the beacon and published.
//
// If either the head or finality fetch fails the prior snapshot is left
// untouched.  The ancestor walk, by contrast, is best-effort and tolerates
// a truncated view due to header request failures.
func (s *Service) refreshState(ctx context.Context, blockID string) error {
	// We use an errgroup to allow for concurrent requests.
	eg, ctx := errgroup.WithContext(ctx)

	var (
		head                 *apiv1.BeaconBlockHeader
		justified, finalized phase0.Checkpoint
	)

	// Fetch latest finality.
	eg.Go(func() error {
		var fetchErr error

		justified, finalized, fetchErr = s.fetchFinality(ctx)
		if fetchErr != nil {
			return errors.Wrap(fetchErr, "failed to fetch finality")
		}

		return nil
	})

	// If no head was provided we fetch the latest head, otherwise we fetch the
	// requested block.
	if blockID == "" {
		blockID = "head"
	}

	eg.Go(func() error {
		var fetchErr error

		head, fetchErr = s.fetchHeader(ctx, blockID)
		if fetchErr != nil {
			return errors.Wrap(fetchErr, "failed to fetch head")
		}

		return nil
	})

	// Wait for fetches to finish.
	if err := eg.Wait(); err != nil {
		return errors.Wrap(err, "refresh state failed")
	}

	prev := s.state.Load()
	headMsg := head.Header.Message

	// Fast path: the new head is a direct descendent of the previous head
	// The previous set of ancestors are still canonical, so reuse them and
	// skip fetching them again.
	if prev != nil && headMsg.ParentRoot == prev.root {
		chain := make(map[phase0.Root]phase0.Slot, s.ancestorTolerance+1)
		chain[head.Root] = headMsg.Slot
		retainRecentAncestors(chain, prev.chain, s.ancestorTolerance)

		s.publishSnapshot(head.Root, headMsg.Slot, justified, finalized, chain)

		return nil
	}

	// Slow path (reorg, gap or bootstrap): go ahead and publish the new head
	// and fetch the ancestors in the background.
	s.publishSnapshot(head.Root, headMsg.Slot, justified, finalized,
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

		// Publish a fresh snapshot as we fetch each ancestor.
		s.publishSnapshot(head.Root, headMsg.Slot, justified, finalized, chain)
	}

	return nil
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

// publishSnapshot atomically stores a new snapshot and updates the head-slot
// metric.  Each call replaces the whole snapshot, so a single Load always
// yields a consistent head, chain and finality triple.
func (s *Service) publishSnapshot(
	root phase0.Root,
	slot phase0.Slot,
	justified, finalized phase0.Checkpoint,
	chain map[phase0.Root]phase0.Slot,
) {
	s.state.Store(&snapshot{
		chain:      chain,
		root:       root,
		slot:       slot,
		lastUpdate: time.Now(),
		justified:  justified,
		finalized:  finalized,
	})

	s.monitor.HeadTrackerHeadSlot(uint64(slot))
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

	// Refresh our view of the chain using this block as the latest head.
	if err := s.refreshState(ctx, blockID); err != nil {
		s.log.Debug().Err(err).Msg("State refresh after head event failed")
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
			// It also acts as a finality backup for empty slots that emit no
			// head event.
			if err := s.refreshState(ctx, headBlockID); err != nil {
				s.log.Debug().Err(err).Msg("Periodic state refresh failed")
			}

			// Publish the age of the cached view.
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
