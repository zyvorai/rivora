// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package gatewayapi

import (
	"fmt"
	"sync"
	"testing"

	"github.com/zyvorai/rivora/internal/config"
)

// Different Gateways reconcile on concurrent workers and share the reconciler's
// installed map. Run under -race: an unguarded map is reported here, and in
// production is a "concurrent map read and map write" crash of rivorad.
func TestRemoveAllForIsSafeAcrossConcurrentWorkers(t *testing.T) {
	r := &Reconciler{plane: newFakeDataplane(), installed: map[string][]string{}}

	const gateways = 24
	var wg sync.WaitGroup
	for round := 0; round < 4; round++ {
		for i := 0; i < gateways; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				desired := []desiredVIP{{VIP: config.VIP{
					Address: fmt.Sprintf("10.0.2.%d", i+1), Port: 80, Protocol: config.ProtoTCP, Mode: config.ModeNAT,
					Backends: []config.Backend{{Address: fmt.Sprintf("10.1.0.%d", i+1), Port: 8080}},
				}}}
				if err := r.removeAllFor(fmt.Sprintf("ns/gw-%d", i), desired); err != nil {
					t.Errorf("removeAllFor: %v", err)
				}
			}()
		}
	}
	wg.Wait()

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.installed) != gateways {
		t.Errorf("installed tracks %d gateways, want %d", len(r.installed), gateways)
	}
}
