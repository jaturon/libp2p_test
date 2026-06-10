// p2p-db relay — minimal libp2p relay for browser peers.
//
// Differences from cmd/gateway:
//   - WebSocket only (no TCP/QUIC) — browsers need WS
//   - No DHT, no mDNS — not needed for relay-only role
//   - GossipSub peer scoring disabled — new/reconnecting browsers
//     must not be penalised and silently dropped
//   - Subscribes to p2p-db app topics so it routes notes between browsers
//   - PEER_RELAYS flag to connect other relay instances (cluster mode)
//   - /api/info also returns cluster peers so browsers discover all relays
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	multiaddr "github.com/multiformats/go-multiaddr"
)

// ── key persistence ────────────────────────────────────────────────────────────
// Stable peer ID means browsers can cache the relay circuit address.
// If the key file is missing a new key is generated and saved.

func loadOrCreateKey(path string) (crypto.PrivKey, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return crypto.UnmarshalPrivateKey(data)
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		return nil, err
	}
	data, err = crypto.MarshalPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return nil, err
	}
	log.Printf("Generated new key → %s", path)
	return priv, nil
}

// ── main ───────────────────────────────────────────────────────────────────────

func main() {
	wsPort  := flag.Int("ws",     4012, "WebSocket listen port (browsers connect here)")
	apiPort := flag.Int("api",    4010, "HTTP port for /api/info and /healthz")
	extIP   := flag.String("extip",  "", "Public IP to announce (for NAT/firewall)")
	extPort := flag.Int("extport",  0,  "Public WS port to announce (default = -ws)")
	extWSSHost := flag.String("extwss-host", "",
		"Hostname of a TLS-terminating reverse proxy in front of -ws (e.g. local-ai-home). "+
			"When set, an additional /dns4/<host>/tcp/<extwss-port>/wss address is announced "+
			"so HTTPS browser pages can dial the relay without mixed-content errors.")
	extWSSPort := flag.Int("extwss-port", 4013, "Public port of the TLS-terminating reverse proxy (used with -extwss-host)")
	keyFile := flag.String("keyfile", "./relay.key", "Ed25519 key file for stable peer ID")
	topicsFlag := flag.String("topics",
		"p2p-db-notes-v1,_peer-discovery._p2p._pubsub",
		"Comma-separated GossipSub topics to subscribe to")
	peerRelaysFlag := flag.String("peer-relays", "",
		"Comma-separated multiaddrs of other relay nodes to cluster with")
	flag.Parse()

	topics := splitTrim(*topicsFlag)
	peerRelayAddrs := splitTrim(*peerRelaysFlag)

	// Also read PEER_RELAYS from env (same as Node.js relay convention)
	if env := os.Getenv("PEER_RELAYS"); env != "" {
		peerRelayAddrs = append(peerRelayAddrs, splitTrim(env)...)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ── connection manager ─────────────────────────────────────────────────────
	cm, err := connmgr.NewConnManager(50, 256, connmgr.WithGracePeriod(30*time.Second))
	if err != nil {
		log.Fatalf("connmgr: %v", err)
	}

	// ── build host ─────────────────────────────────────────────────────────────
	announcedPort := *wsPort
	if *extPort != 0 {
		announcedPort = *extPort
	}

	hostOpts := []libp2p.Option{
		// WebSocket only — browsers cannot open raw TCP connections
		libp2p.ListenAddrStrings(
			fmt.Sprintf("/ip4/0.0.0.0/tcp/%d/ws", *wsPort),
		),

		// Announce public address when running behind NAT or a reverse proxy
		libp2p.AddrsFactory(func(addrs []multiaddr.Multiaddr) []multiaddr.Multiaddr {
			if *extIP == "" {
				return addrs
			}
			s := fmt.Sprintf("/ip4/%s/tcp/%d/ws", *extIP, announcedPort)
			ma, err := multiaddr.NewMultiaddr(s)
			if err != nil {
				return addrs
			}
			// Return only the public address(es) so browsers don't try docker-internal IPs
			out := []multiaddr.Multiaddr{ma}

			// Also announce a wss address via the TLS-terminating reverse proxy,
			// so HTTPS pages can dial the relay (plain ws:// is blocked as
			// mixed content from an https:// origin).
			if *extWSSHost != "" {
				wssMa, err := multiaddr.NewMultiaddr(
					fmt.Sprintf("/dns4/%s/tcp/%d/wss", *extWSSHost, *extWSSPort),
				)
				if err == nil {
					out = append(out, wssMa)
				}
			}
			return out
		}),

		// Circuit relay server — gives each browser a /p2p-circuit/ address
		libp2p.EnableRelayService(),

		libp2p.ConnectionManager(cm),
		libp2p.DisableRelay(), // disable client-side relay (we ARE the relay)
	}

	priv, err := loadOrCreateKey(*keyFile)
	if err != nil {
		log.Fatalf("key: %v", err)
	}
	hostOpts = append(hostOpts, libp2p.Identity(priv))

	h, err := libp2p.New(hostOpts...)
	if err != nil {
		log.Fatalf("create host: %v", err)
	}
	defer h.Close()

	log.Printf("Relay peer ID: %s", h.ID())
	fmt.Println("\nListening on:")
	for _, addr := range h.Addrs() {
		fmt.Printf("  %s/p2p/%s\n", addr, h.ID())
	}
	fmt.Println()

	// ── GossipSub ──────────────────────────────────────────────────────────────
	// Peer scoring is DISABLED (no WithPeerScore option) — browsers that are new,
	// reconnecting, or behind NAT must not be silently dropped.
	// FloodPublish ensures messages reach all mesh peers even when mesh is small.
	gparams := pubsub.DefaultGossipSubParams()
	gparams.D               = 4                      // target mesh degree
	gparams.Dlo             = 1                      // min before grafting
	gparams.Dhi             = 8                      // max before pruning
	gparams.HeartbeatInterval = 500 * time.Millisecond

	gs, err := pubsub.NewGossipSub(ctx, h,
		pubsub.WithFloodPublish(true),
		pubsub.WithGossipSubParams(gparams),
	)
	if err != nil {
		log.Fatalf("GossipSub: %v", err)
	}

	// Subscribe to all configured topics so the relay is IN the mesh.
	// Without this, browsers on different relays cannot exchange messages.
	for _, t := range topics {
		topic, err := gs.Join(t)
		if err != nil {
			log.Fatalf("join topic %q: %v", t, err)
		}
		if _, err := topic.Subscribe(); err != nil {
			log.Fatalf("subscribe %q: %v", t, err)
		}
		log.Printf("Subscribed to topic: %s", t)
	}

	// ── HTTP API ───────────────────────────────────────────────────────────────
	mux := http.NewServeMux()
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "application/json")
		addrs := make([]string, 0, len(h.Addrs()))
		for _, a := range h.Addrs() {
			addrs = append(addrs, fmt.Sprintf("%s/p2p/%s", a, h.ID()))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"peer_id": h.ID().String(),
			"addrs":   addrs,
			"peers":   peerRelayAddrs, // let browsers discover the full cluster
		})
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	go func() {
		addr := fmt.Sprintf(":%d", *apiPort)
		log.Printf("API server on %s", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("API server: %v", err)
		}
	}()

	// ── connection logging ─────────────────────────────────────────────────────
	h.Network().Notify(&network.NotifyBundle{
		ConnectedF: func(_ network.Network, c network.Conn) {
			log.Printf("[+] %s via %s", c.RemotePeer().ShortString(), c.RemoteMultiaddr())
		},
		DisconnectedF: func(_ network.Network, c network.Conn) {
			log.Printf("[-] %s", c.RemotePeer().ShortString())
		},
	})

	// ── cluster: connect to peer relays ───────────────────────────────────────
	// When relays are connected they form a GossipSub mesh: browsers on
	// different relays can sync without knowing about each other directly.
	dialPeerRelay := func(addrStr string) {
		ma, err := multiaddr.NewMultiaddr(addrStr)
		if err != nil {
			log.Printf("[cluster] bad addr %q: %v", addrStr, err)
			return
		}
		pi, err := peer.AddrInfoFromP2pAddr(ma)
		if err != nil {
			log.Printf("[cluster] addr missing /p2p: %q: %v", addrStr, err)
			return
		}
		if err := h.Connect(ctx, *pi); err != nil {
			log.Printf("[cluster] could not reach %s: %v", addrStr, err)
			return
		}
		log.Printf("[cluster] connected to peer relay: %s", addrStr)
	}

	if len(peerRelayAddrs) > 0 {
		log.Printf("Connecting to %d peer relay(s)…", len(peerRelayAddrs))
		for _, addr := range peerRelayAddrs {
			go dialPeerRelay(addr)
		}

		// Reconnect loop — self-heals if a peer relay restarts
		go func() {
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					connected := make(map[peer.ID]bool)
					for _, p := range h.Network().Peers() {
						connected[p] = true
					}
					for _, addr := range peerRelayAddrs {
						ma, err := multiaddr.NewMultiaddr(addr)
						if err != nil {
							continue
						}
						pi, err := peer.AddrInfoFromP2pAddr(ma)
						if err != nil {
							continue
						}
						if !connected[pi.ID] {
							log.Printf("[cluster] reconnecting to %s", addr)
							go dialPeerRelay(addr)
						}
					}
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	// Periodically log peer count
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				n := len(h.Network().Peers())
				if n > 0 {
					log.Printf("[peers] connected: %d", n)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	log.Println("Relay running — Ctrl+C to stop")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("Shutting down relay.")
}

func splitTrim(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
