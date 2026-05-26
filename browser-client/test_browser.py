#!/usr/bin/env python3
"""
Browser integration test: Node.js gateway ↔ browser client ↔ Go peer.

Sequence:
  1. Node.js gateway starts (TCP 4001, WS 4002, API 4000)
  2. Vite dev server starts (port 5173)
  3. Browser (Playwright/Chromium) connects via WebSocket auto-discovered from /api/info
  4. Browser sends a chat message
  5. Go peer (connected separately) receives the message
  6. Go peer sends a reply
  7. Browser receives and displays the reply
"""

import subprocess, time, json, urllib.request, sys, signal, os
from playwright.sync_api import sync_playwright, expect

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
NODE_SERVER = os.path.join(ROOT, "node-server")
BROWSER_CLIENT = os.path.join(ROOT, "browser-client")
GO_PEER = os.path.join(ROOT, "bin", "peer")

procs = []

def start(cmd, cwd=None, **kw):
    p = subprocess.Popen(cmd, cwd=cwd, **kw)
    procs.append(p)
    return p

def cleanup(*_):
    for p in procs:
        p.terminate()
    for p in procs:
        try: p.wait(timeout=3)
        except: p.kill()
    sys.exit(0)

signal.signal(signal.SIGINT, cleanup)
signal.signal(signal.SIGTERM, cleanup)

def wait_for_port(host, port, timeout=15):
    import socket
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            with socket.create_connection((host, port), timeout=1):
                return True
        except OSError:
            time.sleep(0.3)
    raise TimeoutError(f"Port {host}:{port} not ready after {timeout}s")

def api_info(port=4000):
    with urllib.request.urlopen(f"http://127.0.0.1:{port}/api/info", timeout=5) as r:
        return json.loads(r.read())

# ── 1. Start Node.js gateway ──────────────────────────────────────────────────
print("Starting Node.js gateway…")
gw_proc = start(
    ["node", "gateway.js", "--tcp", "4001", "--ws", "4002", "--api", "4000", "--nick", "nodejs-gw"],
    cwd=NODE_SERVER,
    stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
)
wait_for_port("127.0.0.1", 4000)
info = api_info()
print(f"Gateway peer ID: {info['peer_id']}")

ws_addr = next((a for a in info["addrs"] if "/ws" in a and "127.0.0.1" in a), None)
tcp_addr = next((a for a in info["addrs"] if "/tcp/" in a and "/ws" not in a and "127.0.0.1" in a), None)
print(f"WebSocket addr: {ws_addr}")
print(f"TCP addr:       {tcp_addr}")

# ── 2. Start Vite dev server ──────────────────────────────────────────────────
print("\nStarting Vite dev server…")
vite_proc = start(
    ["node_modules/.bin/vite", "--host", "127.0.0.1", "--port", "5173"],
    cwd=BROWSER_CLIENT,
    stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
)
wait_for_port("127.0.0.1", 5173)
print("Vite ready on :5173")

# ── 3. Start Go peer ──────────────────────────────────────────────────────────
print("\nStarting Go peer (go-bob)…")
go_peer_log = open("/tmp/go_peer_browser_test.log", "w")
go_proc = start(
    [GO_PEER, "-gateway", tcp_addr, "-nick", "go-bob"],
    stdin=subprocess.PIPE,
    stdout=go_peer_log, stderr=go_peer_log,
)
time.sleep(2)  # let peer connect and join gossipsub mesh

# ── 4. Browser test ───────────────────────────────────────────────────────────
print("\nLaunching browser…")
with sync_playwright() as pw:
    browser = pw.chromium.launch(
        headless=True,
        args=["--no-sandbox", "--disable-dev-shm-usage"],
    )
    ctx = browser.new_context(
        # Allow mixed content (plain WS from HTTP page) and ignore TLS errors.
        ignore_https_errors=True,
    )
    page = ctx.new_page()

    # Capture browser console to stdout.
    page.on("console", lambda m: print(f"  [browser console] {m.type}: {m.text}"))
    page.on("pageerror", lambda e: print(f"  [browser error] {e}"))

    print("Navigating to http://127.0.0.1:5173/libp2p/")
    page.goto("http://127.0.0.1:5173/libp2p/", wait_until="networkidle")

    # Wait for auto-discovery to fill the address field.
    print("Waiting for gateway address auto-discovery…")
    addr_input = page.locator("#addr-input")
    expect(addr_input).not_to_be_empty(timeout=8000)
    discovered = addr_input.input_value()
    print(f"  Auto-discovered: {discovered}")
    assert "/ws" in discovered, f"Expected a WebSocket address, got: {discovered}"

    # Fill nickname.
    page.fill("#nick-input", "browser-alice")

    # Click Connect and wait for success in event log.
    print("Clicking Connect…")
    page.click("#connect-btn")

    print("Waiting for 'Connected to gateway!' in event log…")
    log_el = page.locator("#log")
    expect(log_el).to_contain_text("Connected to gateway!", timeout=15000)
    print("  ✓ Browser connected to Node.js gateway via WebSocket")

    # Check status dot turned green.
    expect(page.locator("#status-label")).to_have_text("Connected")
    print("  ✓ Status label: Connected")

    # Give GossipSub a moment to form mesh.
    time.sleep(2)

    # Browser sends a message.
    BROWSER_MSG = "hello from the browser!"
    print(f"\nBrowser sending: '{BROWSER_MSG}'")
    page.fill("#msg-input", BROWSER_MSG)
    page.keyboard.press("Enter")

    # Verify message appears in the chat (self-echo).
    expect(page.locator("#messages")).to_contain_text(BROWSER_MSG, timeout=5000)
    print("  ✓ Browser message echoed in chat")

    # Give GossipSub time to propagate to Go peer.
    time.sleep(2)

    # Go peer sends a reply to the browser.
    PEER_MSG = "hello back from go-bob!"
    print(f"\nGo peer sending: '{PEER_MSG}'")
    go_proc.stdin.write((PEER_MSG + "\n").encode())
    go_proc.stdin.flush()

    # Wait for Go peer's reply to appear in the browser.
    print("Waiting for Go peer's reply in browser chat…")
    expect(page.locator("#messages")).to_contain_text(PEER_MSG, timeout=10000)
    print("  ✓ Browser received Go peer's reply")

    # Final screenshot of log for evidence.
    log_text = log_el.inner_text()
    print(f"\nEvent log:\n{log_text}")

    peer_count = page.locator("#peer-count").inner_text()
    print(f"Peer count badge: {peer_count}")

    browser.close()

# ── Summary ───────────────────────────────────────────────────────────────────
go_peer_log.flush()
with open("/tmp/go_peer_browser_test.log") as f:
    go_log = f.read()
print("\n══ Go peer log ══════════════════════════════")
# Show lines mentioning the browser message.
for line in go_log.splitlines():
    if any(k in line for k in ["browser-alice", "hello", "connected", "Chat room", "You"]):
        print(" ", line)

print("\n✓ All assertions passed — Node.js gateway ↔ browser ↔ Go peer works.")
cleanup()
