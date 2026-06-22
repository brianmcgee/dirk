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

package prometheus

import (
	"github.com/prometheus/client_golang/prometheus"
)

func (s *Service) setupHeadTrackerMetrics() error {
	s.headTrackerChecks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "dirk",
		Subsystem: "headtracker",
		Name:      "checks_total",
		Help:      "The number of local head consistency checks, by operation and result.",
	}, []string{"operation", "result"})
	if err := prometheus.Register(s.headTrackerChecks); err != nil {
		return err
	}

	s.headTrackerHeadSlot = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "dirk",
		Subsystem: "headtracker",
		Name:      "head_slot",
		Help:      "The slot of the local beacon node's most recently observed head.",
	})
	if err := prometheus.Register(s.headTrackerHeadSlot); err != nil {
		return err
	}

	s.headTrackerRefreshAge = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "dirk",
		Subsystem: "headtracker",
		Name:      "refresh_age_seconds",
		Help:      "Seconds since the local beacon view was last refreshed from the beacon node, via a head event or the periodic poll.  The staleness check denies signing once this exceeds the configured threshold.",
	})
	if err := prometheus.Register(s.headTrackerRefreshAge); err != nil {
		return err
	}

	s.headTrackerHeadEventDelay = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "dirk",
		Subsystem: "headtracker",
		Name:      "head_event_delay_seconds",
		Help:      "Seconds from the start of a slot until the local beacon's head event for it was received.",
		Buckets: []float64{
			0.1, 0.25, 0.5, 0.75, 1.0, 1.5, 2.0, 2.5, 3.0, 3.5, 4.0, 5.0, 6.0,
		},
	})
	return prometheus.Register(s.headTrackerHeadEventDelay)
}

// HeadTrackerCheck records the outcome of a single check.
func (s *Service) HeadTrackerCheck(operation string, result string) {
	s.headTrackerChecks.WithLabelValues(operation, result).Inc()
}

// HeadTrackerHeadSlot records the slot of the current local head.
func (s *Service) HeadTrackerHeadSlot(slot uint64) {
	s.headTrackerHeadSlot.Set(float64(slot))
}

// HeadTrackerRefreshAge records the seconds since the local beacon view was
// last refreshed.
func (s *Service) HeadTrackerRefreshAge(seconds float64) {
	s.headTrackerRefreshAge.Set(seconds)
}

// HeadTrackerHeadEventDelay records the seconds from the start of a slot until
// its head event was received.
func (s *Service) HeadTrackerHeadEventDelay(seconds float64) {
	s.headTrackerHeadEventDelay.Observe(seconds)
}
