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
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAndCheckParameters(t *testing.T) {
	tests := []struct {
		name     string
		params   []Parameter
		err      string
		expected parameters
	}{
		{
			name: "AddressMissing",
			err:  "no beacon node address specified",
		},
		{
			name: "RequestTimeoutZero",
			params: []Parameter{
				WithAddress("localhost:5052"),
				WithRequestTimeout(0),
			},
			err: "request timeout must be positive",
		},
		{
			name: "StalenessThresholdZero",
			params: []Parameter{
				WithAddress("localhost:5052"),
				WithStalenessThreshold(0),
			},
			err: "staleness threshold must be positive",
		},
		{
			name: "AncestorToleranceZero",
			params: []Parameter{
				WithAddress("localhost:5052"),
				WithAncestorTolerance(0),
			},
			err: "ancestor tolerance must be at least 1",
		},
		{
			name: "SecondsPerSlotZero",
			params: []Parameter{
				WithAddress("localhost:5052"),
				WithSecondsPerSlot(0),
			},
			err: "seconds per slot must be positive",
		},
		{
			name: "SlotsPerEpochZero",
			params: []Parameter{
				WithAddress("localhost:5052"),
				WithSlotsPerEpoch(0),
			},
			err: "slots per epoch must be at least 1",
		},
		{
			name: "GoodDefaults",
			params: []Parameter{
				WithAddress("localhost:5052"),
			},
			expected: parameters{
				logLevel:                  zerolog.GlobalLevel(),
				address:                   "localhost:5052",
				requestTimeout:            10 * time.Second,
				slotsPerEpoch:             32,
				secondsPerSlot:            12 * time.Second,
				ancestorTolerance:         4,
				stalenessThreshold:        24 * time.Second,
				attestationDeadlineOffset: 3500 * time.Millisecond,
			},
		},
		{
			name: "GoodWithOverrides",
			params: []Parameter{
				WithAddress("localhost:5052"),
				WithRequestTimeout(5 * time.Second),
				WithStalenessThreshold(30 * time.Second),
				WithAncestorTolerance(8),
				WithSecondsPerSlot(6 * time.Second),
				WithSlotsPerEpoch(16),
			},
			expected: parameters{
				logLevel:                  zerolog.GlobalLevel(),
				address:                   "localhost:5052",
				requestTimeout:            5 * time.Second,
				slotsPerEpoch:             16,
				secondsPerSlot:            6 * time.Second,
				ancestorTolerance:         8,
				stalenessThreshold:        30 * time.Second,
				attestationDeadlineOffset: 3500 * time.Millisecond,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p, err := parseAndCheckParameters(test.params...)
			if test.err == "" {
				require.NoError(t, err)
				require.NotNil(t, p)
				assert.Equal(t, test.expected, *p)
			} else {
				require.EqualError(t, err, test.err)
			}
		})
	}
}
