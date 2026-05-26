// Peer node: connects to the gateway via circuit relay, discovers other peers,
// and participates in a GossipSub chat room over stdin/stdout.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	ma "github.com/multiformats/go-multiaddr"

	"github.com/user/libp2p_test/pkg/chat"
	"github.com/user/libp2p_test/pkg/discovery"
)

func main() {
	gatewayAddr := flag.String("gateway", "",
		"Full gateway multiaddr, e.g. /ip4/1.2.3.4/tcp/4001/p2p/<peerID>")
	topicName := flag.String("topic", "libp2p_test-chat", "GossipSub chat topic")
	nick := flag.String("nick", "", "Chat nickname (default: first 8 chars of peer ID)")
	keyFile := flag.String("keyfile", "", "Path to persist Ed25519 private key (generated on first run)")
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Parse the gateway address before creating the host so we can pass it
	// to EnableAutoRelayWithStaticRelays at construction time.
	var relayPeers []peer.AddrInfo
	if *gatewayAddr != "" {
		maddr, err := ma.NewMultiaddr(*gatewayAddr)
		if err != nil {
			log.Fatalf("invalid -gateway address: %v", err)
		}
		pi, err := peer.AddrInfoFromP2pAddr(maddr)
		if err != nil {
			log.Fatalf("parse gateway peer info: %v", err)
		}
		relayPeers = append(relayPeers, *pi)
	}

	var identityOpt libp2p.Option
	if *keyFile != "" {
		privKey, err := loadOrCreateKey(*keyFile)
		if err != nil {
			log.Fatalf("key file: %v", err)
		}
		identityOpt = libp2p.Identity(privKey)
	}

	cm, err := connmgr.NewConnManager(50, 200, connmgr.WithGracePeriod(30*time.Second))
	if err != nil {
		log.Fatal(err)
	}

	hostOpts := []libp2p.Option{
		libp2p.ListenAddrStrings(
			"/ip4/0.0.0.0/tcp/0",
			"/ip4/0.0.0.0/udp/0/quic-v1",
		),
		libp2p.EnableNATService(),
		libp2p.NATPortMap(),
		libp2p.ConnectionManager(cm),
	}
	if identityOpt != nil {
		hostOpts = append(hostOpts, identityOpt)
	}

	// If a gateway was provided, use it as a static relay so this peer can
	// receive connections even behind NAT.  Otherwise just enable the relay
	// client protocol without specifying a server.
	if len(relayPeers) > 0 {
		hostOpts = append(hostOpts, libp2p.EnableAutoRelayWithStaticRelays(relayPeers))
	} else {
		hostOpts = append(hostOpts, libp2p.EnableRelay())
	}

	h, err := libp2p.New(hostOpts...)
	if err != nil {
		log.Fatalf("create host: %v", err)
	}
	defer h.Close()

	peerNick := *nick
	if peerNick == "" {
		peerNick = h.ID().String()[:8]
	}

	log.Printf("Peer ID:  %s", h.ID())
	log.Printf("Nick:     %s", peerNick)

	// Start GossipSub before making any connections so it sees the gateway
	// connection as "new" and immediately opens a /meshsub/ stream to it.
	gs, err := pubsub.NewGossipSub(ctx, h)
	if err != nil {
		log.Fatalf("GossipSub: %v", err)
	}

	room, err := chat.JoinRoom(ctx, gs, h.ID(), peerNick, *topicName)
	if err != nil {
		log.Fatalf("join room: %v", err)
	}

	// Connect to gateway now that GossipSub is running — it will open a
	// /meshsub/ stream on this fresh connection.
	if len(relayPeers) > 0 {
		log.Printf("Connecting to gateway %s …", relayPeers[0].ID.ShortString())
		if err := h.Connect(ctx, relayPeers[0]); err != nil {
			log.Fatalf("gateway connection failed: %v", err)
		}
		log.Println("Connected to gateway.")
	}

	// Bootstrap DHT from the gateway (or IPFS bootstrap nodes if no gateway).
	kadDHT, err := discovery.NewDHT(ctx, h, relayPeers, dht.ModeClient)
	if err != nil {
		log.Printf("DHT (non-fatal): %v", err)
		kadDHT = nil
	}

	// mDNS — instant LAN discovery, works without a gateway.
	if err := discovery.StartMDNS(ctx, h, "libp2p_test"); err != nil {
		log.Printf("mDNS (non-fatal): %v", err)
	}

	if kadDHT != nil {
		discovery.Advertise(ctx, kadDHT, discovery.RendezvousString)
		discovery.FindPeers(ctx, h, kadDHT, discovery.RendezvousString)
	}

	fmt.Printf("\nChat room : %s\n", *topicName)
	fmt.Printf("You       : %s\n", room.Nick())
	fmt.Println("Type a message and press Enter. Ctrl+C to quit.\n")

	// Print incoming messages.
	go func() {
		for {
			select {
			case msg := <-room.Messages:
				fmt.Printf("\r[%s] %s: %s\n> ", msg.Timestamp.Format("15:04:05"), msg.SenderNick, msg.Body)
			case <-ctx.Done():
				return
			}
		}
	}()

	// Read stdin and publish.
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		fmt.Print("> ")
		for scanner.Scan() {
			text := strings.TrimSpace(scanner.Text())
			if text == "" {
				fmt.Print("> ")
				continue
			}
			if err := room.Publish(text); err != nil {
				log.Printf("publish: %v", err)
			}
			fmt.Print("> ")
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Println("\nBye!")
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
