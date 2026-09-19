// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Command testpeer is a minimal BGP router for scripts/selftest-bgp.sh: it listens on port 179,
// accepts a session from one neighbour, and prints what it sees, one line per event, so a shell
// script can assert on it. It is a test fixture, not something rivora ships.
//
//	STATE <session state>            whenever the session's state changes
//	ROUTE <prefix> <communities>     for each route in its RIB, re-printed every second
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"
)

func main() {
	asn := flag.Uint("asn", 65002, "this router's AS")
	routerID := flag.String("router-id", "2.2.2.2", "router id")
	neighbor := flag.String("neighbor", "", "the address the neighbour connects from")
	neighborASN := flag.Uint("neighbor-asn", 65001, "the neighbour's AS")
	password := flag.String("password", "", "TCP MD5 password for the neighbour")
	flag.Parse()
	if *neighbor == "" {
		fmt.Fprintln(os.Stderr, "-neighbor is required")
		os.Exit(2)
	}

	s := server.NewBgpServer()
	go s.Serve()
	ctx := context.Background()
	if err := s.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{Asn: uint32(*asn), RouterId: *routerID}}); err != nil {
		fmt.Fprintln(os.Stderr, "start bgp:", err)
		os.Exit(1)
	}
	err := s.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{
		Conf: &api.PeerConf{NeighborAddress: *neighbor, PeerAsn: uint32(*neighborASN), AuthPassword: *password},
		// Multihop, so this side is not what limits the session: the test is about the other end.
		EbgpMultihop: &api.EbgpMultihop{Enabled: true, MultihopTtl: 16},
		Transport:    &api.Transport{PassiveMode: true},
		AfiSafis: []*api.AfiSafi{
			{Config: &api.AfiSafiConfig{Family: &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST}}},
			{Config: &api.AfiSafiConfig{Family: &api.Family{Afi: api.Family_AFI_IP6, Safi: api.Family_SAFI_UNICAST}}},
		},
	}})
	if err != nil {
		fmt.Fprintln(os.Stderr, "add peer:", err)
		os.Exit(1)
	}

	last := ""
	for {
		var state string
		_ = s.ListPeer(ctx, &api.ListPeerRequest{Address: *neighbor}, func(p *api.Peer) {
			state = strings.TrimPrefix(p.State.SessionState.String(), "SESSION_STATE_")
		})
		if state != last {
			fmt.Println("STATE", state)
			last = state
		}
		var lines []string
		_ = s.ListPath(apiutil.ListPathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Family: bgp.RF_IPv4_UC},
			func(nlri bgp.NLRI, paths []*apiutil.Path) {
				if len(paths) == 0 {
					return
				}
				var comms []string
				for _, a := range paths[0].Attrs {
					if c, ok := a.(*bgp.PathAttributeCommunities); ok {
						for _, v := range c.Value {
							comms = append(comms, fmt.Sprintf("%d:%d", v>>16, v&0xffff))
						}
					}
				}
				lines = append(lines, fmt.Sprintf("ROUTE %s [%s]", nlri.String(), strings.Join(comms, " ")))
			})
		sort.Strings(lines)
		for _, l := range lines {
			fmt.Println(l)
		}
		time.Sleep(time.Second)
	}
}
