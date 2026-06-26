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
	"testing"
	"time"

	"github.com/attestantio/dirk/services/headtracker"
	"github.com/attestantio/go-eth2-client/api"
	apiv1 "github.com/attestantio/go-eth2-client/api/v1"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

// rootAt returns a deterministic test root whose first byte equals i.
func rootAt(i byte) phase0.Root {
	var r phase0.Root
	r[0] = i
	return r
}

// newTestService builds a Service with synthetic state suitable for exercising
// the check methods.  The cached chain holds the head (slot 100, root 0x01)
// and three ancestors back to slot 97.  currentSlot derives from genesisTime
// so "now" maps to slot 100.
func newTestService() *Service {
	const (
		headSlot       = phase0.Slot(100)
		slotsPerEpoch  = uint64(32)
		secondsPerSlot = 12 * time.Second
	)

	s := &Service{
		monitor:            &noopMonitor{},
		requestTimeout:     10 * time.Second,
		ancestorTolerance:  4,
		stalenessThreshold: 24 * time.Second,
		slotsPerEpoch:      slotsPerEpoch,
		secondsPerSlot:     secondsPerSlot,
		genesisTime:        time.Now().Add(-time.Duration(headSlot) * secondsPerSlot),
	}

	s.state.Store(&snapshot{
		chain: map[phase0.Root]phase0.Slot{
			rootAt(0x01): 100,
			rootAt(0x02): 99,
			rootAt(0x03): 98,
			rootAt(0x04): 97,
		},
		root:       rootAt(0x01),
		slot:       headSlot,
		lastUpdate: time.Now(),
		justified:  phase0.Checkpoint{Epoch: 3, Root: rootAt(0xaa)},
		finalized:  phase0.Checkpoint{Epoch: 2, Root: rootAt(0xbb)},
	})
	return s
}

// markStale backdates the head update so that staleness checks fail.
func markStale(s *Service) {
	stale := *s.state.Load()
	stale.lastUpdate = time.Now().Add(-2 * s.stalenessThreshold)
	s.state.Store(&stale)
}

// validAttestation builds attestation data that newTestService approves.
func validAttestation(headRoot phase0.Root, justified phase0.Checkpoint) *headtracker.AttestationData {
	return &headtracker.AttestationData{
		Slot:            100,
		BeaconBlockRoot: headRoot,
		Source:          justified,
		Target:          phase0.Checkpoint{Epoch: 3, Root: rootAt(0xcc)},
	}
}

// validProposal builds proposal data that newTestService approves.
func validProposal(parentRoot phase0.Root) *headtracker.ProposalData {
	return &headtracker.ProposalData{
		Slot:       100,
		ParentRoot: parentRoot,
	}
}

func TestCheckAttestation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(s *Service, data *headtracker.AttestationData)
		err    string
	}{
		{
			name: "Approves",
		},
		{
			name: "ApprovesRecentAncestor",
			mutate: func(_ *Service, data *headtracker.AttestationData) {
				data.BeaconBlockRoot = rootAt(0x03) // Two slots behind head, inside tolerance.
			},
		},
		{
			name: "DeniesStale",
			mutate: func(s *Service, _ *headtracker.AttestationData) {
				markStale(s)
			},
			err: "stale",
		},
		{
			name: "DeniesUnknownBlockRoot",
			mutate: func(_ *Service, data *headtracker.AttestationData) {
				data.BeaconBlockRoot = rootAt(0xff) // Not in the cached chain.
			},
			err: "not on the local head's recent chain",
		},
		{
			name: "DeniesSourceEpochMismatch",
			mutate: func(s *Service, data *headtracker.AttestationData) {
				data.Source.Epoch = s.state.Load().justified.Epoch + 1
			},
			err: "source checkpoint",
		},
		{
			name: "DeniesSourceRootMismatch",
			mutate: func(_ *Service, data *headtracker.AttestationData) {
				data.Source.Root = rootAt(0xde) // Matches the epoch but not the local justified root.
			},
			err: "source checkpoint",
		},
		{
			name: "DeniesTargetEpochTooFarBehind",
			mutate: func(_ *Service, data *headtracker.AttestationData) {
				data.Target.Epoch = 1 // Local current epoch is 3 (slot 100 / 32); allowed window 2..4.
			},
			err: "target epoch",
		},
		{
			name: "DeniesTargetEpochTooFarAhead",
			mutate: func(_ *Service, data *headtracker.AttestationData) {
				data.Target.Epoch = 10
			},
			err: "target epoch",
		},
		{
			name: "ApprovesTargetPreviousEpoch",
			mutate: func(_ *Service, data *headtracker.AttestationData) {
				data.Target.Epoch = 2 // currentEpoch-1, lower edge of the accepted window.
			},
		},
		{
			name: "ApprovesTargetNextEpoch",
			mutate: func(_ *Service, data *headtracker.AttestationData) {
				data.Target.Epoch = 4 // currentEpoch+1, upper edge of the accepted window.
			},
		},
		{
			name: "DeniesTargetEpochJustAhead",
			mutate: func(_ *Service, data *headtracker.AttestationData) {
				data.Target.Epoch = 5 // currentEpoch+2, first epoch past the window.
			},
			err: "target epoch",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newTestService()
			data := validAttestation(s.state.Load().root, s.state.Load().justified)

			if test.mutate != nil {
				test.mutate(s, data)
			}

			err := s.CheckAttestation(context.Background(), data)
			if test.err == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), test.err)
			}
		})
	}
}

func TestCheckAttestationBlockRootWait(t *testing.T) {
	const lateRoot = 0x05

	t.Run("ApprovesOnceRootImportedWithinTolerance", func(t *testing.T) {
		s := newTestService()
		// newTestService places slot 100 at ~now, so a 2s delay leaves ample
		// headroom before this slot's attestation deadline.
		s.blockRootPollInterval = 10 * time.Millisecond
		s.maxAttestationDelay = 2 * time.Second

		data := validAttestation(rootAt(lateRoot), s.state.Load().justified)

		// Simulate the local beacon importing the late block shortly after the
		// request arrives: publish a new head snapshot that includes its root.
		go func() {
			time.Sleep(40 * time.Millisecond)

			next := *s.state.Load()

			chain := make(map[phase0.Root]phase0.Slot, len(next.chain)+1)
			for r, sl := range next.chain {
				chain[r] = sl
			}
			chain[rootAt(lateRoot)] = 101

			next.chain = chain
			next.root = rootAt(lateRoot)
			next.slot = 101
			next.lastUpdate = time.Now()

			s.state.Store(&next)
		}()

		assert.NoError(t, s.CheckAttestation(context.Background(), data))
	})

	t.Run("DeniesWhenRootNeverImportedBeforeTolerance", func(t *testing.T) {
		s := newTestService()
		// Push the deadline 80ms past slot start so the wait is short but real.
		s.genesisTime = time.Now().Add(-100 * 12 * time.Second)
		s.blockRootPollInterval = 10 * time.Millisecond
		s.maxAttestationDelay = 80 * time.Millisecond

		data := validAttestation(rootAt(0xff), s.state.Load().justified)

		err := s.CheckAttestation(context.Background(), data)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not on the local head's recent chain")
	})

	t.Run("DeniesImmediatelyWhenToleranceDisabled", func(t *testing.T) {
		s := newTestService() // blockRootTolerance defaults to 0.
		data := validAttestation(rootAt(0xff), s.state.Load().justified)

		start := time.Now()
		err := s.CheckAttestation(context.Background(), data)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not on the local head's recent chain")
		assert.Less(t, time.Since(start), 50*time.Millisecond, "must not wait when tolerance is disabled")
	})

	t.Run("DeniesWhenRequestContextCancelled", func(t *testing.T) {
		s := newTestService()
		// A long deadline so the context cancellation is what ends the wait.
		s.genesisTime = time.Now().Add(-100 * 12 * time.Second)
		s.maxAttestationDelay = 10 * time.Second
		s.blockRootPollInterval = 10 * time.Millisecond

		data := validAttestation(rootAt(0xff), s.state.Load().justified)

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(40 * time.Millisecond)
			cancel()
		}()

		start := time.Now()
		err := s.CheckAttestation(ctx, data)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not on the local head's recent chain")
		assert.Less(t, time.Since(start), 2*time.Second, "must abandon the wait when the request is cancelled")
	})

	t.Run("DeniesImmediatelyWhenDeadlineAlreadyPassed", func(t *testing.T) {
		s := newTestService()
		// Place the attested slot a full slot in the past so its deadline has
		// already elapsed by the time the request arrives, even though the wait
		// is enabled.  The wait must short-circuit rather than poll.
		s.genesisTime = time.Now().Add(-101 * 12 * time.Second)
		s.maxAttestationDelay = 4 * time.Second
		s.blockRootPollInterval = 10 * time.Millisecond

		data := validAttestation(rootAt(0xff), s.state.Load().justified)

		start := time.Now()
		err := s.CheckAttestation(context.Background(), data)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not on the local head's recent chain")
		assert.Less(t, time.Since(start), 50*time.Millisecond, "must not poll when the slot deadline is already past")
	})
}

// fakeFinalityProvider is a minimal eth2client.FinalityProvider that returns
// checkpoints keyed by the queries state id and records every state queried.
// When fixed is set it is returned for any state, for tests that do not care
// which state is queried.
type fakeFinalityProvider struct {
	queries []string
	byState map[string]apiv1.Finality
	fixed   *apiv1.Finality
	err     error
}

func (f *fakeFinalityProvider) Finality(_ context.Context, opts *api.FinalityOpts) (*api.Response[*apiv1.Finality], error) {
	f.queries = append(f.queries, opts.State)
	if f.err != nil {
		return nil, f.err
	}

	if f.fixed != nil {
		return &api.Response[*apiv1.Finality]{Data: f.fixed}, nil
	}

	fin, ok := f.byState[opts.State]
	if !ok {
		return nil, fmt.Errorf("no finality configured for state %q", opts.State)
	}

	return &api.Response[*apiv1.Finality]{Data: &fin}, nil
}

// fakeHeadersProvider is a minimal eth2client.BeaconBlockHeadersProvider that
// serves preconfigured headers by block id, records the order of fetches, and
// fires an optional hook before each lookup.
type fakeHeadersProvider struct {
	headers map[string]*apiv1.BeaconBlockHeader
	fetched []string
	onFetch func(blockID string)
}

func (f *fakeHeadersProvider) BeaconBlockHeader(_ context.Context, opts *api.BeaconBlockHeaderOpts) (*api.Response[*apiv1.BeaconBlockHeader], error) {
	f.fetched = append(f.fetched, opts.Block)
	if f.onFetch != nil {
		f.onFetch(opts.Block)
	}

	h, ok := f.headers[opts.Block]
	if !ok {
		return nil, fmt.Errorf("no header configured for block %q", opts.Block)
	}

	return &api.Response[*apiv1.BeaconBlockHeader]{Data: h}, nil
}

// testHeader builds an apiv1 block header with the given root, slot and parent.
func testHeader(root phase0.Root, slot phase0.Slot, parent phase0.Root) *apiv1.BeaconBlockHeader {
	return &apiv1.BeaconBlockHeader{
		Root: root,
		Header: &phase0.SignedBeaconBlockHeader{
			Message: &phase0.BeaconBlockHeader{
				Slot:       slot,
				ParentRoot: parent,
			},
		},
	}
}

// rootHex formats a root as the block id the ancestor walk uses.
func rootHex(r phase0.Root) string {
	return fmt.Sprintf("%#x", r[:])
}

// TestRefreshStatePublishesHeadBeforeAncestorWalk verifies the new head is
// visible in the published snapshot before the ancestor walk runs, so a bounded
// attestation wait need not block on the serial ancestor fetches.
func TestRefreshStatePublishesHeadBeforeAncestorWalk(t *testing.T) {
	const secondsPerSlot = 12 * time.Second

	headRoot := rootAt(0x10)
	parentRoot := rootAt(0x11)
	grandparentRoot := rootAt(0x12)

	headers := &fakeHeadersProvider{
		headers: map[string]*apiv1.BeaconBlockHeader{
			"head":                   testHeader(headRoot, 100, parentRoot),
			rootHex(parentRoot):      testHeader(parentRoot, 99, grandparentRoot),
			rootHex(grandparentRoot): testHeader(grandparentRoot, 98, rootAt(0x13)),
		},
	}

	s := &Service{
		monitor:            &noopMonitor{},
		headersProvider:    headers,
		requestTimeout:     time.Second,
		ancestorTolerance:  4,
		stalenessThreshold: 24 * time.Second,
		slotsPerEpoch:      32,
		secondsPerSlot:     secondsPerSlot,
		genesisTime:        time.Now().Add(-100 * secondsPerSlot),
	}

	// When the walk fetches the first ancestor, the head must already be
	// published.
	var headVisibleDuringWalk bool
	headers.onFetch = func(blockID string) {
		if blockID == rootHex(parentRoot) {
			if st := s.state.Load(); st != nil {
				_, headVisibleDuringWalk = st.chain[headRoot]
			}
		}
	}

	rq := require.New(t)
	rq.NoError(s.refreshHead(context.Background(), "head"))
	rq.True(headVisibleDuringWalk, "head root must be published before the ancestor walk")

	st := s.state.Load()
	rq.NotNil(st)

	as := assert.New(t)
	as.Equal(headRoot, st.root)
	for _, r := range []phase0.Root{headRoot, parentRoot, grandparentRoot} {
		_, ok := st.chain[r]
		as.Truef(ok, "final chain must contain %#x", r)
	}
}

// newRefreshTestService builds a Service wired to the supplied fakes, with the
// wall clock placed so the current slot is headSlot.
func newRefreshTestService(headers *fakeHeadersProvider, finality *fakeFinalityProvider, headSlot phase0.Slot) *Service {
	const secondsPerSlot = 12 * time.Second

	return &Service{
		monitor:            &noopMonitor{},
		headersProvider:    headers,
		finalityProvider:   finality,
		requestTimeout:     time.Second,
		ancestorTolerance:  4,
		stalenessThreshold: 24 * time.Second,
		slotsPerEpoch:      32,
		secondsPerSlot:     secondsPerSlot,
		genesisTime:        time.Now().Add(-time.Duration(headSlot) * secondsPerSlot),
	}
}

// TestRefreshStateSkipsKnownAncestors verifies that when the new head directly
// extends our previous head the prior window is reused without re-fetching any
// ancestors, and is bounded to the configured tolerance.
func TestRefreshStateSkipsKnownAncestors(t *testing.T) {
	prevRoot := rootAt(0x21)
	headRoot := rootAt(0x20)
	oldestRoot := rootAt(0x25)

	headers := &fakeHeadersProvider{
		headers: map[string]*apiv1.BeaconBlockHeader{
			"head": testHeader(headRoot, 101, prevRoot),
			// Ancestor headers are intentionally absent: any walk would error.
		},
	}

	justified := phase0.Checkpoint{Epoch: 3, Root: rootAt(0xaa)}
	finalized := phase0.Checkpoint{Epoch: 2, Root: rootAt(0xbb)}
	finality := &fakeFinalityProvider{fixed: &apiv1.Finality{Justified: &justified, Finalized: &finalized}}

	s := newRefreshTestService(headers, finality, 101)

	// Seed the previous head (slot 100) and its window. The window holds
	// tolerance+1 (5) blocks, so reuse must drop the oldest (slot 96).
	s.state.Store(&snapshot{
		chain: map[phase0.Root]phase0.Slot{
			prevRoot:     100,
			rootAt(0x22): 99,
			rootAt(0x23): 98,
			rootAt(0x24): 97,
			oldestRoot:   96,
		},
		root:       prevRoot,
		slot:       100,
		lastUpdate: time.Now(),
	})

	require.NoError(t, s.refreshHead(context.Background(), "head"))

	as := assert.New(t)
	as.Equal([]string{"head"}, headers.fetched, "only the head must be fetched on a direct extension")

	st := s.state.Load()
	require.NotNil(t, st)
	as.Equal(headRoot, st.root)
	as.Len(st.chain, 5, "window must be bounded to tolerance+1")

	_, ok := st.chain[headRoot]
	as.True(ok, "head must be present")
	_, ok = st.chain[prevRoot]
	as.True(ok, "previous head must be retained as an ancestor")
	_, ok = st.chain[oldestRoot]
	as.False(ok, "oldest entry beyond tolerance must be pruned")
}

// TestRefreshStateWalksAncestorsOnReorg verifies that when the new head does not
// extend our previous head the ancestor walk runs and the stale prior window is
// discarded rather than reused.
func TestRefreshStateWalksAncestorsOnReorg(t *testing.T) {
	staleRoot := rootAt(0x31) // Our previous head, now off-chain.
	headRoot := rootAt(0x30)
	parentRoot := rootAt(0x32)
	a2 := rootAt(0x33)
	a3 := rootAt(0x34)
	a4 := rootAt(0x35)

	headers := &fakeHeadersProvider{
		headers: map[string]*apiv1.BeaconBlockHeader{
			"head":              testHeader(headRoot, 101, parentRoot),
			rootHex(parentRoot): testHeader(parentRoot, 100, a2),
			rootHex(a2):         testHeader(a2, 99, a3),
			rootHex(a3):         testHeader(a3, 98, a4),
			rootHex(a4):         testHeader(a4, 97, rootAt(0x36)),
		},
	}

	justified := phase0.Checkpoint{Epoch: 3, Root: rootAt(0xaa)}
	finalized := phase0.Checkpoint{Epoch: 2, Root: rootAt(0xbb)}
	finality := &fakeFinalityProvider{fixed: &apiv1.Finality{Justified: &justified, Finalized: &finalized}}

	s := newRefreshTestService(headers, finality, 101)

	// Previous head is NOT the new head's parent: a reorg.
	s.state.Store(&snapshot{
		chain:      map[phase0.Root]phase0.Slot{staleRoot: 100},
		root:       staleRoot,
		slot:       100,
		lastUpdate: time.Now(),
	})

	require.NoError(t, s.refreshHead(context.Background(), "head"))

	as := assert.New(t)
	as.Contains(headers.fetched, rootHex(parentRoot), "ancestors must be walked on a reorg")

	st := s.state.Load()
	require.NotNil(t, st)
	_, ok := st.chain[staleRoot]
	as.False(ok, "off-chain prior head must not be retained on a reorg")
	_, ok = st.chain[parentRoot]
	as.True(ok, "walked ancestor must be present")
}

// TestUpdateStateConcurrentMergePreservesBothHalves runs head writers and
// finality writers concurrently, each applying a distinct increment, and
// asserts that no update from either half is lost.
func TestUpdateStateConcurrentMergePreservesBothHalves(t *testing.T) {
	s := &Service{}
	s.state.Store(&snapshot{slot: 1, justified: phase0.Checkpoint{Epoch: 1}})

	const n = 64

	var g errgroup.Group
	for range n {
		g.Go(func() error {
			s.updateState(func(next *snapshot) { next.slot++ })
			return nil
		})
		g.Go(func() error {
			s.updateState(func(next *snapshot) { next.justified.Epoch++ })
			return nil
		})
	}
	require.NoError(t, g.Wait())

	st := s.state.Load()

	assert.Equal(t, phase0.Slot(n+1), st.slot, "every head increment must survive concurrent finality merges")
	assert.Equal(t, phase0.Epoch(n+1), st.justified.Epoch, "every finality increment must survive concurrent head merges")
}

// TestFetchFinalityQueriesCurrentSlotState reproduces the epoch-boundary
// checkpoint regression: when the first slot of an epoch is empty or late the
// raw "head" state still justifies the previous epoch, one behind the source an
// attester derives from the head state advanced to the current slot.  Querying
// by the current slot must surface the advanced (correct) justified checkpoint,
// never the lagging head one.
func TestFetchFinalityQueriesCurrentSlotState(t *testing.T) {
	const (
		currentSlot    = phase0.Slot(64) // First slot of epoch 2.
		slotsPerEpoch  = uint64(32)
		secondsPerSlot = 12 * time.Second
	)

	// "head" lags one epoch behind the slot-advanced view, as it would when the
	// boundary block has not yet been imported locally.
	headJustified := phase0.Checkpoint{Epoch: 0, Root: rootAt(0xa0)}
	slotJustified := phase0.Checkpoint{Epoch: 1, Root: rootAt(0xa1)}
	finalized := phase0.Checkpoint{Epoch: 0, Root: rootAt(0xb0)}

	fake := &fakeFinalityProvider{
		byState: map[string]apiv1.Finality{
			"head": {Justified: &headJustified, Finalized: &finalized},
			"64":   {Justified: &slotJustified, Finalized: &finalized},
		},
	}

	s := &Service{
		monitor:          &noopMonitor{},
		finalityProvider: fake,
		requestTimeout:   10 * time.Second,
		slotsPerEpoch:    slotsPerEpoch,
		secondsPerSlot:   secondsPerSlot,
		genesisTime:      time.Now().Add(-time.Duration(currentSlot) * secondsPerSlot),
	}

	justified, finalizedGot, err := s.fetchFinality(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, fake.queries)

	as := assert.New(t)

	as.Equal("64", fake.queries[0], "finality must be queried at the current slot")
	as.NotContains(fake.queries, "head", "must not query the lagging head state")
	as.Equal(phase0.Epoch(1), justified.Epoch, "must surface the current-slot justified checkpoint")
	as.Equal(slotJustified.Root, justified.Root)
	as.Equal(finalized, finalizedGot)
}

// TestCheckAttestationCheckpointWait covers the epoch-boundary checkpoint race:
// when an attestation's source is exactly one epoch ahead of the cached
// justified checkpoint, CheckAttestation re-queries the local beacon's finality
// before denying, and approves once it advances.
func TestCheckAttestationCheckpointWait(t *testing.T) {
	const (
		secondsPerSlot = 12 * time.Second
		currentSlot    = phase0.Slot(128) // First slot of epoch 4.
	)

	cached := phase0.Checkpoint{Epoch: 2, Root: rootAt(0xa2)}
	advanced := apiv1.Finality{
		Justified: &phase0.Checkpoint{Epoch: 3, Root: rootAt(0xa3)},
		Finalized: &phase0.Checkpoint{Epoch: 2, Root: rootAt(0xb2)},
	}
	stale := apiv1.Finality{
		Justified: &phase0.Checkpoint{Epoch: 2, Root: rootAt(0xa2)},
		Finalized: &phase0.Checkpoint{Epoch: 1, Root: rootAt(0xb1)},
	}

	// build wires a service whose cached justified checkpoint lags at epoch 2,
	// with the head root 0x01 on-chain at the current slot, finality queries
	// answered by served, and the given attestation delay.
	build := func(served *fakeFinalityProvider, delay time.Duration) (*Service, *headtracker.AttestationData) {
		s := &Service{
			monitor:               &noopMonitor{},
			finalityProvider:      served,
			requestTimeout:        time.Second,
			ancestorTolerance:     4,
			stalenessThreshold:    24 * time.Second,
			maxAttestationDelay:   delay,
			blockRootPollInterval: 20 * time.Millisecond,
			slotsPerEpoch:         32,
			secondsPerSlot:        secondsPerSlot,
			genesisTime:           time.Now().Add(-time.Duration(currentSlot) * secondsPerSlot),
		}
		s.state.Store(&snapshot{
			chain:      map[phase0.Root]phase0.Slot{rootAt(0x01): currentSlot},
			root:       rootAt(0x01),
			slot:       currentSlot,
			lastUpdate: time.Now(),
			justified:  cached,
			finalized:  phase0.Checkpoint{Epoch: 1, Root: rootAt(0xb1)},
		})
		data := &headtracker.AttestationData{
			Slot:            currentSlot,
			BeaconBlockRoot: rootAt(0x01),
			Source:          phase0.Checkpoint{Epoch: 3, Root: rootAt(0xa3)}, // one epoch ahead
			Target:          phase0.Checkpoint{Epoch: 4, Root: rootAt(0xcc)},
		}
		return s, data
	}

	t.Run("ApprovesOnceCheckpointAdvancesWithinDeadline", func(t *testing.T) {
		fin := &fakeFinalityProvider{fixed: &advanced}
		s, data := build(fin, 2*time.Second)

		assert.NoError(t, s.CheckAttestation(context.Background(), data))
		assert.NotEmpty(t, fin.queries, "finality must be re-queried on the boundary mismatch")
	})

	t.Run("DeniesWhenCheckpointNeverAdvances", func(t *testing.T) {
		fin := &fakeFinalityProvider{fixed: &stale}
		s, data := build(fin, 150*time.Millisecond)

		start := time.Now()
		err := s.CheckAttestation(context.Background(), data)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "source checkpoint")
		assert.Less(t, time.Since(start), 2*time.Second, "must give up at the deadline")
	})

	t.Run("DeniesImmediatelyWhenSourceMoreThanOneEpochAhead", func(t *testing.T) {
		fin := &fakeFinalityProvider{fixed: &advanced}
		s, data := build(fin, 2*time.Second)
		data.Source = phase0.Checkpoint{Epoch: 4, Root: rootAt(0xa4)} // two epochs ahead

		start := time.Now()
		err := s.CheckAttestation(context.Background(), data)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "source checkpoint")
		assert.Empty(t, fin.queries, "must not re-query for a non-boundary mismatch")
		assert.Less(t, time.Since(start), 50*time.Millisecond, "must deny immediately")
	})

	t.Run("DeniesImmediatelyWhenWaitDisabled", func(t *testing.T) {
		fin := &fakeFinalityProvider{fixed: &advanced}
		s, data := build(fin, 0) // delay 0 disables the wait

		err := s.CheckAttestation(context.Background(), data)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "source checkpoint")
		assert.Empty(t, fin.queries, "a disabled wait must not re-query")
	})
}

func TestCheckProposal(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(s *Service, data *headtracker.ProposalData)
		err    string
	}{
		{
			name: "Approves",
		},
		{
			name: "ApprovesNextSlot",
			mutate: func(_ *Service, data *headtracker.ProposalData) {
				data.Slot = 101
			},
		},
		{
			name: "DeniesStale",
			mutate: func(s *Service, _ *headtracker.ProposalData) {
				markStale(s)
			},
			err: "stale",
		},
		{
			name: "DeniesFutureSlot",
			mutate: func(_ *Service, data *headtracker.ProposalData) {
				data.Slot = 150
			},
			err: "slot",
		},
		{
			name: "DeniesPastSlot",
			mutate: func(_ *Service, data *headtracker.ProposalData) {
				data.Slot = 50
			},
			err: "slot",
		},
		{
			name: "DeniesUnknownParent",
			mutate: func(_ *Service, data *headtracker.ProposalData) {
				data.ParentRoot = rootAt(0xff) // Not in the cached chain.
			},
			err: "parent root",
		},
		{
			name: "ApprovesPreviousSlot",
			mutate: func(_ *Service, data *headtracker.ProposalData) {
				data.Slot = 99 // currentSlot-1, lower edge of the accepted window.
			},
		},
		{
			name: "DeniesSlotJustBehind",
			mutate: func(_ *Service, data *headtracker.ProposalData) {
				data.Slot = 98 // currentSlot-2, first slot below the window.
			},
			err: "slot",
		},
		{
			name: "DeniesSlotJustAhead",
			mutate: func(_ *Service, data *headtracker.ProposalData) {
				data.Slot = 102 // currentSlot+2, first slot above the window.
			},
			err: "slot",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newTestService()
			data := validProposal(s.state.Load().root)

			if test.mutate != nil {
				test.mutate(s, data)
			}

			err := s.CheckProposal(context.Background(), data)
			if test.err == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), test.err)
			}
		})
	}
}

// TestCheckAttestationTargetEpochAtGenesis guards the unsigned-safe target
// window arithmetic.  At epoch 0 the lower bound currentEpoch-1 would underflow
// a uint64, so the check is written as Target.Epoch+1 < currentEpoch.  A target
// of epoch 0 when the chain is in epoch 0 must therefore be accepted, not
// wrongly denied.
func TestCheckAttestationTargetEpochAtGenesis(t *testing.T) {
	const secondsPerSlot = 12 * time.Second

	s := &Service{
		monitor:            &noopMonitor{},
		requestTimeout:     10 * time.Second,
		ancestorTolerance:  4,
		stalenessThreshold: 24 * time.Second,
		slotsPerEpoch:      32,
		secondsPerSlot:     secondsPerSlot,
		// Slot 3 maps to epoch 0.
		genesisTime: time.Now().Add(-3 * secondsPerSlot),
	}

	justified := phase0.Checkpoint{Epoch: 0, Root: rootAt(0xaa)}
	s.state.Store(&snapshot{
		chain:      map[phase0.Root]phase0.Slot{rootAt(0x01): 3},
		root:       rootAt(0x01),
		slot:       3,
		lastUpdate: time.Now(),
		justified:  justified,
		finalized:  phase0.Checkpoint{Epoch: 0, Root: rootAt(0xbb)},
	})

	data := &headtracker.AttestationData{
		Slot:            3,
		BeaconBlockRoot: rootAt(0x01),
		Source:          justified,
		Target:          phase0.Checkpoint{Epoch: 0, Root: rootAt(0xcc)},
	}

	assert.NoError(t, s.CheckAttestation(context.Background(), data))
}
