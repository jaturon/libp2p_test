#!/usr/bin/env node
/**
 * Gateway node (Node.js) — mirrors cmd/gateway/main.go
 *
 * Runs a libp2p node with:
 *   - TCP + WebSocket transports (browser-reachable)
 *   - Circuit relay server (NAT traversal)
 *   - DHT server (bootstrap point)
 *   - mDNS (LAN discovery)
 *   - GossipSub chat on topic "libp2p_test-chat"
 *   - HTTP /api/info on port 4000 (auto-discovery for browser client)
 */

import { createLibp2p } from 'libp2p'
import { tcp } from '@libp2p/tcp'
import { webSockets } from '@libp2p/websockets'
import { noise } from '@chainsafe/libp2p-noise'
import { yamux } from '@libp2p/yamux'
import { identify } from '@libp2p/identify'
import { ping } from '@libp2p/ping'
import { gossipsub } from '@chainsafe/libp2p-gossipsub'
import { kadDHT } from '@libp2p/kad-dht'
import { mdns } from '@libp2p/mdns'
import { circuitRelayServer } from '@libp2p/circuit-relay-v2'
import { multiaddr } from '@multiformats/multiaddr'
import { createServer } from 'http'
import { readFileSync, writeFileSync, existsSync } from 'fs'
import { generateKeyPair, privateKeyFromRaw, privateKeyToProtobuf, privateKeyFromProtobuf } from '@libp2p/crypto/keys'
import process from 'process'

// ── CLI args (matches Go gateway flags) ───────────────────────────────────────

const args = parseArgs(process.argv.slice(2))
const tcpPort  = parseInt(args['tcp']  ?? '4001')
const wsPort   = parseInt(args['ws']   ?? '4002')
const apiPort  = parseInt(args['api']  ?? '4000')
const topicName = args['topic'] ?? 'libp2p_test-chat'
const nick      = args['nick']  ?? 'gateway-node'
const keyFile   = args['keyfile'] ?? ''

function parseArgs(argv) {
  const out = {}
  for (let i = 0; i < argv.length; i++) {
    const m = argv[i].match(/^--?(\w[\w-]*)(?:=(.*))?$/)
    if (m) {
      out[m[1]] = m[2] ?? argv[++i] ?? true
    }
  }
  return out
}

// ── Key persistence (mirrors loadOrCreateKey in Go) ───────────────────────────

async function loadOrCreateKey(path) {
  if (path && existsSync(path)) {
    const raw = readFileSync(path)
    return privateKeyFromProtobuf(raw)
  }
  const key = await generateKeyPair('Ed25519')
  if (path) {
    writeFileSync(path, privateKeyToProtobuf(key))
    console.log(`Generated new key, saved to ${path}`)
  }
  return key
}

// ── Chat helpers (mirrors pkg/chat/chat.go) ────────────────────────────────────

function buildChatMessage(peerId, senderNick, body) {
  return JSON.stringify({
    sender_id: peerId.toString(),
    sender_nick: senderNick,
    body,
    timestamp: new Date().toISOString(),
  })
}

// ── Main ──────────────────────────────────────────────────────────────────────

async function main() {
  const privateKey = await loadOrCreateKey(keyFile)

  const node = await createLibp2p({
    privateKey: keyFile ? privateKey : undefined,
    addresses: {
      listen: [
        `/ip4/0.0.0.0/tcp/${tcpPort}`,
        `/ip4/0.0.0.0/tcp/${wsPort}/ws`,
        `/ip6/::/tcp/${tcpPort}`,
      ],
    },
    transports: [
      tcp(),
      webSockets(),
    ],
    connectionEncrypters: [noise()],
    streamMuxers: [yamux()],
    services: {
      identify: identify(),
      ping: ping(),
      dht: kadDHT({ mode: 'server' }),
      mdns: mdns({ serviceTag: 'libp2p_test' }),
      relay: circuitRelayServer({ advertise: true }),
      pubsub: gossipsub({
        allowPublishToZeroTopicPeers: true,
        floodPublish: true,
        emitSelf: false,
        heartbeatInterval: 500,
        D: 4, Dlo: 1, Dhi: 8,
        // Relaxed scoring so newly-joining peers aren't penalised.
        scoreThresholds: {
          gossipThreshold: -Infinity,
          publishThreshold: -Infinity,
          graylistThreshold: -Infinity,
          acceptPXThreshold: 0,
          opportunisticGraftThreshold: 1,
        },
      }),
    },
  })

  await node.start()

  const peerId = node.peerId
  console.log(`\nGateway peer ID: ${peerId}`)
  console.log('\nListening on:')
  for (const addr of node.getMultiaddrs()) {
    console.log(`  ${addr}`)
  }
  console.log()

  // ── HTTP /api/info ──────────────────────────────────────────────────────────

  if (apiPort) {
    const srv = createServer((req, res) => {
      if (req.url !== '/api/info') {
        res.writeHead(404); res.end(); return
      }
      const addrs = node.getMultiaddrs().map(a => a.toString())
      res.writeHead(200, {
        'Content-Type': 'application/json',
        'Access-Control-Allow-Origin': '*',
      })
      res.end(JSON.stringify({ peer_id: peerId.toString(), addrs }))
    })
    srv.listen(apiPort, () => console.log(`API server listening on :${apiPort}`))
  }

  // ── Connection logging (mirrors network.NotifyBundle) ───────────────────────

  node.addEventListener('peer:connect', evt => {
    console.log(`[conn] connected: ${evt.detail}`)
  })
  node.addEventListener('peer:disconnect', evt => {
    console.log(`[conn] disconnected: ${evt.detail}`)
  })

  // ── GossipSub chat ──────────────────────────────────────────────────────────

  node.services.pubsub.subscribe(topicName)
  node.services.pubsub.addEventListener('message', evt => {
    if (evt.detail.topic !== topicName) return
    try {
      const msg = JSON.parse(new TextDecoder().decode(evt.detail.data))
      const peers = node.services.pubsub.getSubscribers(topicName)
      console.log(`[chat][${new Date().toLocaleTimeString()}] ${msg.sender_nick}: ${msg.body} | topic peers: ${peers.length}`)
    } catch { /* malformed */ }
  })

  console.log(`Joined chat topic "${topicName}" as "${nick}"`)
  console.log('Gateway running — Ctrl+C to stop\n')

  // Periodic topic peer count log (mirrors Go ticker).
  setInterval(() => {
    try {
      const peers = node.services.pubsub.getSubscribers(topicName)
      console.log(`[pubsub] topic peers (${peers.length}): ${peers.map(p => p.toString().slice(0,8)).join(', ')}`)
    } catch { /* topic not ready yet */ }
  }, 5000)

  // Occasionally publish a heartbeat so the mesh stays alive.
  // (Mirrors gateway participating in the room; commented out for quieter logs.)
  // setInterval(async () => {
  //   const data = new TextEncoder().encode(buildChatMessage(peerId, nick, '[gateway heartbeat]'))
  //   await node.services.pubsub.publish(topicName, data).catch(() => {})
  // }, 30_000)

  // ── Graceful shutdown ───────────────────────────────────────────────────────

  const shutdown = async () => {
    console.log('\nShutting down gateway.')
    await node.stop()
    process.exit(0)
  }
  process.on('SIGINT',  shutdown)
  process.on('SIGTERM', shutdown)
}

main().catch(err => { console.error(err); process.exit(1) })
