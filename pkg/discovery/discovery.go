// Package discovery provides mDNS and DHT-based peer discovery for libp2p hosts.
package discovery

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
	"github.com/libp2p/go-libp2p/p2p/discovery/routing"
	util "github.com/libp2p/go-libp2p/p2p/discovery/util"
)

// RendezvousString is the shared key peers use to find each other on the DHT.
const RendezvousString = "libp2p_test-v1"

// mdnsNotifee connects to peers found via mDNS.
type mdnsNotifee struct {
	h   host.Host
	ctx context.Context
}

func (n *mdnsNotifee) HandlePeerFound(pi peer.AddrInfo) {
	log.Printf("[mDNS] found %s", pi.ID.ShortString())
	if err := n.h.Connect(n.ctx, pi); err != nil {
		log.Printf("[mDNS] connect %s: %v", pi.ID.ShortString(), err)
	}
}

// StartMDNS starts local network peer discovery via mDNS.
func StartMDNS(ctx context.Context, h host.Host, serviceTag string) error {
	svc := mdns.NewMdnsService(h, serviceTag, &mdnsNotifee{h: h, ctx: ctx})
	return svc.Start()
}

// NewDHT creates and bootstraps a Kademlia DHT, connecting to any provided bootstrap peers.
// Pass nil bootstrapPeers to act as a root bootstrap node (server mode).
func NewDHT(ctx context.Context, h host.Host, bootstrapPeers []peer.AddrInfo, mode dht.ModeOpt) (*dht.IpfsDHT, error) {
	opts := []dht.Option{dht.Mode(mode)}
	if len(bootstrapPeers) > 0 {
		opts = append(opts, dht.BootstrapPeers(bootstrapPeers...))
	}

	kadDHT, err := dht.New(ctx, h, opts...)
	if err != nil {
		return nil, fmt.Errorf("create dht: %w", err)
	}

	// Connect to bootstrap peers concurrently before bootstrapping.
	var wg sync.WaitGroup
	for _, bp := range bootstrapPeers {
		wg.Add(1)
		go func(p peer.AddrInfo) {
			defer wg.Done()
			if err := h.Connect(ctx, p); err != nil {
				log.Printf("[DHT] bootstrap peer %s: %v", p.ID.ShortString(), err)
			}
		}(bp)
	}
	wg.Wait()

	if err := kadDHT.Bootstrap(ctx); err != nil {
		return nil, fmt.Errorf("dht bootstrap: %w", err)
	}

	log.Printf("[DHT] bootstrapped (mode=%v, bootstrapPeers=%d)", mode, len(bootstrapPeers))
	return kadDHT, nil
}

// Advertise continuously re-announces the rendezvous point so other peers can find us.
func Advertise(ctx context.Context, kadDHT *dht.IpfsDHT, rendezvous string) {
	rd := routing.NewRoutingDiscovery(kadDHT)
	util.Advertise(ctx, rd, rendezvous)
	log.Printf("[DHT] advertising rendezvous %q", rendezvous)
}

// FindPeers discovers peers via DHT rendezvous in a background goroutine, connecting as they appear.
func FindPeers(ctx context.Context, h host.Host, kadDHT *dht.IpfsDHT, rendezvous string) {
	rd := routing.NewRoutingDiscovery(kadDHT)
	go func() {
		for {
			peerCh, err := rd.FindPeers(ctx, rendezvous)
			if err != nil {
				log.Printf("[DHT] FindPeers: %v", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(30 * time.Second):
					continue
				}
			}
			for pi := range peerCh {
				if pi.ID == h.ID() {
					continue
				}
				if h.Network().Connectedness(pi.ID) != network.Connected {
					if err := h.Connect(ctx, pi); err == nil {
						log.Printf("[DHT] connected to %s", pi.ID.ShortString())
					}
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
		}
	}()
}
