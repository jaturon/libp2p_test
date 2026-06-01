.PHONY: all gateway peer p2p-db-relay browser-deps browser browser-dev deps clean \
        run-gateway run-peer run-p2p-db-relay \
        node-deps run-node-gateway run-node-peer run-node-relay

BINDIR   := bin
BROWSER  := browser-client

all: gateway peer p2p-db-relay browser

# ── Go binaries ───────────────────────────────────────────────────────────────

gateway:
	@mkdir -p $(BINDIR)
	go build -o $(BINDIR)/gateway ./cmd/gateway

peer:
	@mkdir -p $(BINDIR)
	go build -o $(BINDIR)/peer ./cmd/peer

# p2p-db relay — WebSocket-only circuit relay + GossipSub routing for browsers
p2p-db-relay:
	@mkdir -p $(BINDIR)
	GOTOOLCHAIN=local go build -ldflags="-s -w" -o $(BINDIR)/p2p-db-relay ./cmd/p2p-db-relay

deps:
	go mod tidy

# ── Browser client ────────────────────────────────────────────────────────────

browser-deps:
	cd $(BROWSER) && npm install

# Build to local dist/, then deploy to /var/www/html/libp2p_test.
# Run `sudo chown -R $$USER /var/www/html/libp2p_test` once to avoid needing sudo here.
browser: browser-deps
	cd $(BROWSER) && npm run build
	sudo mkdir -p /var/www/html/libp2p_test
	sudo cp -r $(BROWSER)/dist/. /var/www/html/libp2p_test/

# Hot-reload dev server (served at http://0.0.0.0:5173).
browser-dev: browser-deps
	cd $(BROWSER) && npm run dev

# ── Clean ─────────────────────────────────────────────────────────────────────

clean:
	rm -rf $(BINDIR) $(BROWSER)/node_modules $(BROWSER)/dist

# ── Quick-start ───────────────────────────────────────────────────────────────

# Terminal 1: start gateway
run-gateway: gateway
	./$(BINDIR)/gateway -tcp 4001 -udp 4001 -ws 4002 -wt 4003

# p2p-db relay: WS on 4012, API on 4010
# Optional: EXTIP=1.2.3.4 PEER_RELAYS=/ip4/.../ws/p2p/...
run-p2p-db-relay: p2p-db-relay
	./$(BINDIR)/p2p-db-relay \
		-ws 4012 -api 4010 \
		$(if $(EXTIP),-extip "$(EXTIP)") \
		$(if $(KEYFILE),-keyfile "$(KEYFILE)",-keyfile relay.key) \
		$(if $(PEER_RELAYS),-peer-relays "$(PEER_RELAYS)")

# Terminal 2+: join as a CLI chat peer
# Usage: make run-peer GATEWAY="/ip4/127.0.0.1/tcp/4001/p2p/<peerID>" NICK=alice
run-peer: peer
	./$(BINDIR)/peer \
		$(if $(GATEWAY),-gateway "$(GATEWAY)") \
		$(if $(NICK),-nick "$(NICK)")

# ── Node.js server ────────────────────────────────────────────────────────────

NODE_DIR := node-server

node-deps:
	cd $(NODE_DIR) && npm install

# Terminal 1: start gateway (mirrors run-gateway but Node.js)
run-node-gateway: node-deps
	cd $(NODE_DIR) && node gateway.js --tcp 4001 --ws 4002 --api 4000 --nick gateway-node

# Node.js p2p-db relay (original JS version)
run-node-relay: node-deps
	cd node-server && node relay.js 2>/dev/null || \
	  (cd ../p2p-db/relay && API_PORT=4010 WS_PORT=4012 node server.js)

# Terminal 2+: join as a CLI chat peer (Node.js)
# Usage: make run-node-peer GATEWAY="/ip4/127.0.0.1/tcp/4001/p2p/<peerID>" NICK=alice
run-node-peer: node-deps
	cd $(NODE_DIR) && node peer.js \
		$(if $(GATEWAY),--gateway "$(GATEWAY)") \
		$(if $(NICK),--nick "$(NICK)")
