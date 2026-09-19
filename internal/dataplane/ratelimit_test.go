// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package dataplane

import "testing"

func TestScaleRateLimitDividesAcrossCPUsWithAFloor(t *testing.T) {
	// A limit smaller than the CPU count must still admit something: a bucket of
	// zero would drop every SYN outright.
	got := scaleRateLimit(1, 1)
	if got.RatePerSec < 1 || got.Burst < 1 || got.Enabled != 1 {
		t.Errorf("scaleRateLimit(1,1) = %+v, want rate and burst floored at 1 and enabled", got)
	}
	if big := scaleRateLimit(1_000_000, 2_000_000); big.RatePerSec > 1_000_000 || big.Burst > 2_000_000 || big.RatePerSec == 0 {
		t.Errorf("scaleRateLimit(1e6, 2e6) = %+v", big)
	}
}
