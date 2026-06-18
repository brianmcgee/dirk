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

package standard_test

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/attestantio/dirk/core"
	"github.com/attestantio/dirk/rules"
	mockrules "github.com/attestantio/dirk/rules/mock"
	"github.com/attestantio/dirk/services/checker"
	mockchecker "github.com/attestantio/dirk/services/checker/mock"
	"github.com/attestantio/dirk/services/fetcher"
	memfetcher "github.com/attestantio/dirk/services/fetcher/mem"
	"github.com/attestantio/dirk/services/headtracker"
	mockheadtracker "github.com/attestantio/dirk/services/headtracker/mock"
	syncmaplocker "github.com/attestantio/dirk/services/locker/syncmap"
	"github.com/attestantio/dirk/services/ruler"
	"github.com/attestantio/dirk/services/ruler/golang"
	"github.com/attestantio/dirk/services/signer"
	standardsigner "github.com/attestantio/dirk/services/signer/standard"
	"github.com/attestantio/dirk/services/unlocker"
	localunlocker "github.com/attestantio/dirk/services/unlocker/local"
	spec "github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	keystorev4 "github.com/wealdtech/go-eth2-wallet-encryptor-keystorev4"
	hd "github.com/wealdtech/go-eth2-wallet-hd/v2"
	scratch "github.com/wealdtech/go-eth2-wallet-store-scratch"
	e2wtypes "github.com/wealdtech/go-eth2-wallet-types/v2"
)

// signingEnv holds the collaborators a signer needs, built once per test with a
// single unlocked account ready to sign.
type signingEnv struct {
	checker     checker.Service
	fetcher     fetcher.Service
	ruler       ruler.Service
	unlocker    unlocker.Service
	credentials *checker.Credentials
	pubKeyOne   []byte
	pubKeyTwo   []byte
}

// newSigningEnv builds the collaborators and an unlocked account.  The ruler is
// the real golang ruler driven by mock rules that approve and persist nothing,
// so signing succeeds without cross-test slashing-protection state.
func newSigningEnv(ctx context.Context, t *testing.T) signingEnv {
	t.Helper()
	rq := require.New(t)

	store := scratch.New()
	encryptor := keystorev4.New()

	seed := []byte{
		0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
		0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
		0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, 0x29, 0x2a, 0x2b, 0x2c, 0x2d, 0x2e, 0x2f,
		0x30, 0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37, 0x38, 0x39, 0x3a, 0x3b, 0x3c, 0x3d, 0x3e, 0x3f,
	}

	wallet, err := hd.CreateWallet(ctx, "Test wallet", []byte("secret"), store, encryptor, seed)
	rq.NoError(err)
	rq.NoError(wallet.(e2wtypes.WalletLocker).Unlock(ctx, []byte("secret")))

	accountOne, err := wallet.(e2wtypes.WalletAccountCreator).CreateAccount(ctx, "Test account 1", []byte("Test account 1 passphrase"))
	rq.NoError(err)
	rq.NoError(accountOne.(e2wtypes.AccountLocker).Unlock(ctx, []byte("Test account 1 passphrase")))

	pubKeyOne := accountOne.PublicKey().Marshal()

	// A second account so the bulk path can sign a batch spanning distinct validators
	accountTwo, err := wallet.(e2wtypes.WalletAccountCreator).CreateAccount(ctx, "Test account 2", []byte("Test account 2 passphrase"))
	rq.NoError(err)
	rq.NoError(accountTwo.(e2wtypes.AccountLocker).Unlock(ctx, []byte("Test account 2 passphrase")))

	pubKeyTwo := accountTwo.PublicKey().Marshal()

	rq.NoError(wallet.(e2wtypes.WalletLocker).Lock(ctx))

	lockerSvc, err := syncmaplocker.New(ctx)
	rq.NoError(err)

	fetcherSvc, err := memfetcher.New(ctx, memfetcher.WithStores([]e2wtypes.Store{store}))
	rq.NoError(err)

	rulerSvc, err := golang.New(ctx, golang.WithLocker(lockerSvc), golang.WithRules(mockrules.New()))
	rq.NoError(err)

	unlockerSvc, err := localunlocker.New(ctx, localunlocker.WithAccountPassphrases([]string{
		"Test account 1 passphrase",
		"Test account 2 passphrase",
	}))
	rq.NoError(err)

	checkerSvc, err := mockchecker.New(zerolog.Disabled)
	rq.NoError(err)

	return signingEnv{
		checker:     checkerSvc,
		fetcher:     fetcherSvc,
		ruler:       rulerSvc,
		unlocker:    unlockerSvc,
		credentials: &checker.Credentials{Client: "client1"},
		pubKeyOne:   pubKeyOne,
		pubKeyTwo:   pubKeyTwo,
	}
}

// _signerSvcWithHeadTracker builds a signer service with an installed head
// tracker, mirroring _signerSvc.
func _signerSvcWithHeadTracker(ctx context.Context,
	checker checker.Service,
	fetcher fetcher.Service,
	ruler ruler.Service,
	unlocker unlocker.Service,
	headTracker headtracker.Service,
) signer.Service {
	signerSvc, err := standardsigner.New(ctx,
		standardsigner.WithChecker(checker),
		standardsigner.WithFetcher(fetcher),
		standardsigner.WithRuler(ruler),
		standardsigner.WithUnlocker(unlocker),
		standardsigner.WithHeadTracker(headTracker))
	if err != nil {
		panic(err)
	}
	return signerSvc
}

// spyRuler records whether RunRules was invoked, approving everything.  It lets
// a test assert that the head tracker check runs before the rules engine: when
// the tracker denies, the rules must never run (and so never persist slashing
// protection state for a request that is rejected).
type spyRule struct {
	called atomic.Bool
}

func (r *spyRule) RunRules(_ context.Context, _ *checker.Credentials, _ string, data []*ruler.RulesData) []rules.Result {
	r.called.CompareAndSwap(false, true)

	results := make([]rules.Result, len(data))
	for i := range results {
		results[i] = rules.APPROVED
	}
	return results
}

func (r *spyRule) wasCalled() bool {
	return r.called.Load()
}

// root32 returns a 32-byte slice filled with b, a distinctive value for
// asserting field translation.
func root32(b byte) []byte {
	return bytes.Repeat([]byte{b}, 32)
}

// specRoot returns a spec.Root filled with b.
func specRoot(b byte) spec.Root {
	var r spec.Root
	for i := range r {
		r[i] = b
	}
	return r
}

// validAttestation returns a well-formed attestation request that drives the
// signer through to the head tracker check.
func validAttestation() *rules.SignBeaconAttestationData {
	return &rules.SignBeaconAttestationData{
		Domain:          make([]byte, 32),
		BeaconBlockRoot: make([]byte, 32),
		Source:          &rules.Checkpoint{Epoch: 1, Root: make([]byte, 32)},
		Target:          &rules.Checkpoint{Epoch: 2, Root: make([]byte, 32)},
	}
}

// validProposal returns a well-formed proposal request that drives the signer
// through to the head tracker check.
func validProposal() *rules.SignBeaconProposalData {
	return &rules.SignBeaconProposalData{
		Domain:     make([]byte, 32),
		Slot:       10,
		ParentRoot: make([]byte, 32),
		StateRoot:  make([]byte, 32),
		BodyRoot:   make([]byte, 32),
	}
}

// TestHeadTrackerIntegration verifies that an installed head tracker is
// consulted by the signer and that its verdict determines the outcome, and that
// without a tracker the signing path is unchanged.
func TestHeadTrackerIntegration(t *testing.T) {
	ctx := context.Background()
	env := newSigningEnv(ctx, t)

	attestationDenier := mockheadtracker.New()
	attestationDenier.AttestationErr = errors.New("local beacon view is stale")

	proposalDenier := mockheadtracker.New()
	proposalDenier.ProposalErr = errors.New("parent root not on local head's chain")

	tests := []struct {
		name      string
		signer    signer.Service
		operation string
		res       core.Result
	}{
		{
			name:      "AttestationApprovesWithoutTracker",
			signer:    _signerSvc(ctx, env.checker, env.fetcher, env.ruler, env.unlocker),
			operation: "attestation",
			res:       core.ResultSucceeded,
		},
		{
			name:      "AttestationApprovedByTracker",
			signer:    _signerSvcWithHeadTracker(ctx, env.checker, env.fetcher, env.ruler, env.unlocker, mockheadtracker.New()),
			operation: "attestation",
			res:       core.ResultSucceeded,
		},
		{
			name:      "AttestationDeniedByTracker",
			signer:    _signerSvcWithHeadTracker(ctx, env.checker, env.fetcher, env.ruler, env.unlocker, attestationDenier),
			operation: "attestation",
			res:       core.ResultDenied,
		},
		{
			name:      "ProposalApprovesWithoutTracker",
			signer:    _signerSvc(ctx, env.checker, env.fetcher, env.ruler, env.unlocker),
			operation: "proposal",
			res:       core.ResultSucceeded,
		},
		{
			name:      "ProposalApprovedByTracker",
			signer:    _signerSvcWithHeadTracker(ctx, env.checker, env.fetcher, env.ruler, env.unlocker, mockheadtracker.New()),
			operation: "proposal",
			res:       core.ResultSucceeded,
		},
		{
			name:      "ProposalDeniedByTracker",
			signer:    _signerSvcWithHeadTracker(ctx, env.checker, env.fetcher, env.ruler, env.unlocker, proposalDenier),
			operation: "proposal",
			res:       core.ResultDenied,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var res core.Result
			switch test.operation {
			case "attestation":
				res, _ = test.signer.SignBeaconAttestation(ctx, env.credentials, "", env.pubKeyOne, validAttestation())
			case "proposal":
				res, _ = test.signer.SignBeaconProposal(ctx, env.credentials, "", env.pubKeyOne, validProposal())
			}
			assert.Equal(t, test.res, res)
		})
	}
}

// TestHeadTrackerBulkAttestation exercises the head tracker integration on the
// bulk SignBeaconAttestations path, which consults the tracker once per entry
// (from multiple goroutines) and rejects the whole batch on a single denial.
func TestHeadTrackerBulkAttestation(t *testing.T) {
	ctx := context.Background()
	env := newSigningEnv(ctx, t)

	t.Run("ApprovesBatch", func(t *testing.T) {
		ht := mockheadtracker.New()
		signerSvc := _signerSvcWithHeadTracker(ctx, env.checker, env.fetcher, env.ruler, env.unlocker, ht)

		data := []*rules.SignBeaconAttestationData{validAttestation(), validAttestation()}
		results, sigs := signerSvc.SignBeaconAttestations(
			ctx, env.credentials,
			[]string{"", ""},
			[][]byte{env.pubKeyOne, env.pubKeyTwo},
			data,
		)

		assert.Equal(t, []core.Result{core.ResultSucceeded, core.ResultSucceeded}, results)
		assert.Len(t, sigs, 2)
		assert.Len(t, ht.Attestations, 2, "tracker consulted once per entry")
	})

	t.Run("DeniesWholeBatch", func(t *testing.T) {
		ht := mockheadtracker.New()
		ht.AttestationErr = errors.New("local beacon view is stale")
		signerSvc := _signerSvcWithHeadTracker(ctx, env.checker, env.fetcher, env.ruler, env.unlocker, ht)

		data := []*rules.SignBeaconAttestationData{validAttestation(), validAttestation()}
		results, _ := signerSvc.SignBeaconAttestations(ctx, env.credentials,
			[]string{"", ""}, [][]byte{env.pubKeyOne, env.pubKeyTwo}, data)

		assert.Equal(t, []core.Result{core.ResultDenied, core.ResultDenied}, results,
			"a single denial must reject the whole batch")
	})
}

// TestHeadTrackerRunsBeforeRules pins the ordering invariant: the head tracker
// is consulted before the rules engine, so a tracker denial short-circuits the
// request and the rules (which persist slashing protection on approval) never
// run.  This holds for both attestations and proposals.
func TestHeadTrackerRunsBeforeRules(t *testing.T) {
	ctx := context.Background()
	env := newSigningEnv(ctx, t)

	denier := func(operation string) headtracker.Service {
		ht := mockheadtracker.New()
		switch operation {
		case "attestation":
			ht.AttestationErr = errors.New("denied")
		case "proposal":
			ht.ProposalErr = errors.New("denied")
		}
		return ht
	}

	tests := []struct {
		name       string
		operation  string
		tracker    headtracker.Service
		wantResult core.Result
		wantRules  bool
	}{
		{"AttestationDeniedSkipsRules", "attestation", denier("attestation"), core.ResultDenied, false},
		{"AttestationApprovedRunsRules", "attestation", mockheadtracker.New(), core.ResultSucceeded, true},
		{"ProposalDeniedSkipsRules", "proposal", denier("proposal"), core.ResultDenied, false},
		{"ProposalApprovedRunsRules", "proposal", mockheadtracker.New(), core.ResultSucceeded, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spy := &spyRule{}
			signerSvc := _signerSvcWithHeadTracker(ctx, env.checker, env.fetcher, spy, env.unlocker, test.tracker)

			var res core.Result
			switch test.operation {
			case "attestation":
				res, _ = signerSvc.SignBeaconAttestation(ctx, env.credentials, "", env.pubKeyOne, validAttestation())
			case "proposal":
				res, _ = signerSvc.SignBeaconProposal(ctx, env.credentials, "", env.pubKeyOne, validProposal())
			}

			assert.Equal(t, test.wantResult, res)
			assert.Equal(t, test.wantRules, spy.wasCalled(), "rules should run only when the tracker approves")
		})
	}
}

// TestHeadTrackerRequestTranslation asserts the signer translates the signing
// request into the head tracker's view correctly: a transposed or omitted field
// would have the tracker check the wrong data while the signature still goes
// out.  The capturing mock records what it was actually handed.
func TestHeadTrackerRequestTranslation(t *testing.T) {
	ctx := context.Background()
	env := newSigningEnv(ctx, t)

	t.Run("Attestation", func(t *testing.T) {
		ht := mockheadtracker.New()
		signerSvc := _signerSvcWithHeadTracker(ctx, env.checker, env.fetcher, env.ruler, env.unlocker, ht)

		data := &rules.SignBeaconAttestationData{
			Domain:          make([]byte, 32),
			Slot:            100,
			CommitteeIndex:  3,
			BeaconBlockRoot: root32(0x33),
			Source:          &rules.Checkpoint{Epoch: 7, Root: root32(0x11)},
			Target:          &rules.Checkpoint{Epoch: 9, Root: root32(0x22)},
		}

		res, _ := signerSvc.SignBeaconAttestation(ctx, env.credentials, "", env.pubKeyOne, data)
		require.Equal(t, core.ResultSucceeded, res)

		require.Len(t, ht.Attestations, 1)
		got := ht.Attestations[0]

		as := assert.New(t)

		as.Equal(spec.Slot(100), got.Slot)
		as.Equal(specRoot(0x33), got.BeaconBlockRoot)
		as.Equal(spec.Epoch(7), got.Source.Epoch)
		as.Equal(specRoot(0x11), got.Source.Root)
		as.Equal(spec.Epoch(9), got.Target.Epoch)
		as.Equal(specRoot(0x22), got.Target.Root)
	})

	t.Run("Proposal", func(t *testing.T) {
		ht := mockheadtracker.New()
		signerSvc := _signerSvcWithHeadTracker(ctx, env.checker, env.fetcher, env.ruler, env.unlocker, ht)

		data := &rules.SignBeaconProposalData{
			Domain:     make([]byte, 32),
			Slot:       77,
			ParentRoot: root32(0x44),
			StateRoot:  make([]byte, 32),
			BodyRoot:   make([]byte, 32),
		}

		res, _ := signerSvc.SignBeaconProposal(ctx, env.credentials, "", env.pubKeyOne, data)
		require.Equal(t, core.ResultSucceeded, res)

		require.Len(t, ht.Proposals, 1)
		got := ht.Proposals[0]
		assert.Equal(t, spec.Slot(77), got.Slot)
		assert.Equal(t, specRoot(0x44), got.ParentRoot)
	})
}
