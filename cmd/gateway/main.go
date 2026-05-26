// Gateway node: circuit relay server + DHT bootstrap + mDNS + GossipSub chat.
// Browsers connect via WebSocket or WebTransport; NAT-ed peers connect via circuit relay.
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
	"syscall"
	"time"

	logging "github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/event"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	multiaddr "github.com/multiformats/go-multiaddr"

	"github.com/user/libp2p_test/pkg/chat"
	"github.com/user/libp2p_test/pkg/discovery"
)

// multiaddrsFlag is a repeatable -bootstrap flag that accumulates peer multiaddrs.
type multiaddrsFlag []string

func (f *multiaddrsFlag) String() string { return fmt.Sprintf("%v", *f) }
func (f *multiaddrsFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

// parseBootstrapPeers converts multiaddr strings (e.g. /ip4/.../tcp/.../p2p/12D3Koo...)
// into peer.AddrInfo values suitable for DHT bootstrap.
func parseBootstrapPeers(addrs []string) ([]peer.AddrInfo, error) {
	peers := make([]peer.AddrInfo, 0, len(addrs))
	for _, s := range addrs {
		ma, err := multiaddr.NewMultiaddr(s)
		if err != nil {
			return nil, fmt.Errorf("invalid bootstrap multiaddr %q: %w", s, err)
		}
		pi, err := peer.AddrInfoFromP2pAddr(ma)
		if err != nil {
			return nil, fmt.Errorf("bootstrap multiaddr %q missing /p2p component: %w", s, err)
		}
		peers = append(peers, *pi)
	}
	return peers, nil
}

func loadOrCreateKey(path string) (crypto.PrivKey, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return crypto.UnmarshalPrivateKey(data)
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	privKey, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		return nil, err
	}
	data, err = crypto.MarshalPrivateKey(privKey)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return nil, err
	}
	log.Printf("Generated new key, saved to %s", path)
	return privKey, nil
}

func main() {
	tcpPort := flag.Int("tcp", 4001, "TCP listen port (plain + WebSocket)")
	udpPort := flag.Int("udp", 4001, "QUIC/UDP listen port")
	wsPort := flag.Int("ws", 4002, "WebSocket listen port (browser-friendly)")
	wtPort := flag.Int("wt", 4003, "WebTransport UDP port (browser-friendly)")
	extIP := flag.String("extip", "", "External/public IP to announce (for DMZ or port-forwarding)")
	topicName := flag.String("topic", "libp2p_test-chat", "GossipSub chat topic")
	nick := flag.String("nick", "gateway", "Chat nickname for this node")
	keyFile := flag.String("keyfile", "", "Path to persist Ed25519 private key")
	apiPort := flag.Int("api", 4000, "HTTP API port for /api/info (set 0 to disable)")
	debugPubsub := flag.Bool("debug-pubsub", false, "Enable debug logging for pubsub")
	var bootstrapAddrs multiaddrsFlag
	flag.Var(&bootstrapAddrs, "bootstrap", "Peer multiaddr to bootstrap from (repeatable, e.g. /ip4/1.2.3.4/tcp/4001/p2p/12D3Koo...)")
	flag.Parse()

	if *debugPubsub {
		logging.SetLogLevel("pubsub", "debug")
	}

	bootstrapPeers, err := parseBootstrapPeers(bootstrapAddrs)
	if err != nil {
		log.Fatalf("bootstrap peers: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Generous connection limits for a public gateway.
	cm, err := connmgr.NewConnManager(100, 400, connmgr.WithGracePeriod(time.Minute))
	if err != nil {
		log.Fatalf("connmgr: %v", err)
	}

	hostOpts := []libp2p.Option{
		libp2p.ListenAddrStrings(
			// Standard transports
			fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", *tcpPort),
			fmt.Sprintf("/ip4/0.0.0.0/udp/%d/quic-v1", *udpPort),
			fmt.Sprintf("/ip6/::/tcp/%d", *tcpPort),
			fmt.Sprintf("/ip6/::/udp/%d/quic-v1", *udpPort),
			// Browser-accessible transports
			fmt.Sprintf("/ip4/0.0.0.0/tcp/%d/ws", *wsPort),
			fmt.Sprintf("/ip4/0.0.0.0/udp/%d/quic-v1/webtransport", *wtPort),
		),
		// Announce external IP when running behind DMZ / port-forwarding.
		libp2p.AddrsFactory(func(addrs []multiaddr.Multiaddr) []multiaddr.Multiaddr {
			if *extIP == "" {
				return addrs
			}
			extra := []string{
				fmt.Sprintf("/ip4/%s/tcp/%d", *extIP, *tcpPort),
				fmt.Sprintf("/ip4/%s/udp/%d/quic-v1", *extIP, *udpPort),
				fmt.Sprintf("/ip4/%s/tcp/%d/ws", *extIP, *wsPort),
				fmt.Sprintf("/ip4/%s/udp/%d/quic-v1/webtransport", *extIP, *wtPort),
			}
			for _, s := range extra {
				ma, err := multiaddr.NewMultiaddr(s)
				if err == nil {
					addrs = append(addrs, ma)
				}
			}
			return addrs
		}),
		// Act as a circuit relay server so NAT-ed peers can reach each other.
		libp2p.EnableRelayService(),
		// Respond to AutoNAT probes from other peers.
		libp2p.EnableNATService(),
		// UPnP / NAT-PMP port mapping (best-effort).
		libp2p.NATPortMap(),
		libp2p.ConnectionManager(cm),
	}
	if *keyFile != "" {
		privKey, err := loadOrCreateKey(*keyFile)
		if err != nil {
			log.Fatalf("key file: %v", err)
		}
		hostOpts = append(hostOpts, libp2p.Identity(privKey))
	}
	h, err := libp2p.New(hostOpts...)
	if err != nil {
		log.Fatalf("create host: %v", err)
	}
	defer h.Close()

	log.Printf("Gateway peer ID: %s", h.ID())
	fmt.Println("\nListening on:")
	for _, addr := range h.Addrs() {
		fmt.Printf("  %s/p2p/%s\n", addr, h.ID())
	}
	fmt.Println()

	// HTTP /api/info — lets the browser client auto-discover the gateway multiaddrs.
	if *apiPort != 0 {
		mux := http.NewServeMux()
		mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Content-Type", "application/json")
			addrs := make([]string, 0)
			for _, a := range h.Addrs() {
				addrs = append(addrs, fmt.Sprintf("%s/p2p/%s", a, h.ID()))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"peer_id": h.ID().String(),
				"addrs":   addrs,
			})
		})
		go func() {
			addr := fmt.Sprintf(":%d", *apiPort)
			log.Printf("API server listening on %s", addr)
			if err := http.ListenAndServe(addr, mux); err != nil {
				log.Printf("API server: %v", err)
			}
		}()
	}

	// Log protocols each peer advertises after identify completes.
	sub, err := h.EventBus().Subscribe(new(event.EvtPeerIdentificationCompleted))
	if err != nil {
		log.Printf("identify subscribe (non-fatal): %v", err)
	} else {
		go func() {
			defer sub.Close()
			for e := range sub.Out() {
				evt := e.(event.EvtPeerIdentificationCompleted)
				protos, _ := h.Peerstore().GetProtocols(evt.Peer)
				log.Printf("[identify] %s protos: %v", evt.Peer.ShortString(), protos)
			}
		}()
	}

	// Log all peer connect/disconnect events at the transport layer.
	h.Network().Notify(&network.NotifyBundle{
		ConnectedF: func(_ network.Network, c network.Conn) {
			log.Printf("[conn] connected: %s via %s", c.RemotePeer(), c.RemoteMultiaddr())
		},
		DisconnectedF: func(_ network.Network, c network.Conn) {
			log.Printf("[conn] disconnected: %s", c.RemotePeer())
		},
	})

	// DHT server: other peers bootstrap from us and find peers through us.
	kadDHT, err := discovery.NewDHT(ctx, h, bootstrapPeers, dht.ModeServer)
	if err != nil {
		log.Fatalf("DHT: %v", err)
	}

	// mDNS for zero-config LAN discovery.
	if err := discovery.StartMDNS(ctx, h, "libp2p_test"); err != nil {
		log.Printf("mDNS (non-fatal): %v", err)
	}

	// Announce ourselves and continuously connect to newly discovered peers.
	discovery.Advertise(ctx, kadDHT, discovery.RendezvousString)
	discovery.FindPeers(ctx, h, kadDHT, discovery.RendezvousString)

	// GossipSub — gateway participates so it keeps the mesh healthy.
	gparams := pubsub.DefaultGossipSubParams()
	gparams.D = 4
	gparams.Dlo = 1
	gparams.Dhi = 8
	gparams.HeartbeatInterval = 500 * time.Millisecond

	gs, err := pubsub.NewGossipSub(ctx, h,
		pubsub.WithFloodPublish(true),
		pubsub.WithGossipSubParams(gparams),
	)
	if err != nil {
		log.Fatalf("GossipSub: %v", err)
	}

	room, err := chat.JoinRoom(ctx, gs, h.ID(), *nick, *topicName)
	if err != nil {
		log.Fatalf("join room: %v", err)
	}
	log.Printf("Joined chat topic %q as %q", *topicName, room.Nick())
	log.Println("Gateway running — Ctrl+C to stop\n")

	// Log chat messages and topic peer list for debugging.
	go func() {
		for {
			select {
			case msg := <-room.Messages:
				peers := room.Peers()
				log.Printf("[chat][%s] %s: %s | topic peers: %d %v",
					msg.Timestamp.Format("15:04:05"), msg.SenderNick, msg.Body, len(peers), peers)
			case <-ctx.Done():
				return
			}
		}
	}()

	// Periodically log who the gateway sees subscribed to the topic.
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				peers := room.Peers()
				log.Printf("[pubsub] topic peers (%d): %v", len(peers), peers)
			case <-ctx.Done():
				return
			}
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("Shutting down gateway.")
}
