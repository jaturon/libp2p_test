.PHONY: all gateway peer browser-deps browser browser-dev deps clean \
        run-gateway run-peer \
        node-deps run-node-gateway run-node-peer

BINDIR   := bin
BROWSER  := browser-client

all: gateway peer browser

# ── Go binaries ───────────────────────────────────────────────────────────────

gateway:
	@mkdir -p $(BINDIR)
	go build -o $(BINDIR)/gateway ./cmd/gateway

peer:
	@mkdir -p $(BINDIR)
	go build -o $(BINDIR)/peer ./cmd/peer

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

# Terminal 2+: join as a CLI chat peer (Node.js)
# Usage: make run-node-peer GATEWAY="/ip4/127.0.0.1/tcp/4001/p2p/<peerID>" NICK=alice
run-node-peer: node-deps
	cd $(NODE_DIR) && node peer.js \
		$(if $(GATEWAY),--gateway "$(GATEWAY)") \
		$(if $(NICK),--nick "$(NICK)")
