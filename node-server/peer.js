#!/usr/bin/env node
/**
 * Peer node (Node.js) — mirrors cmd/peer/main.go
 *
 * Connects to a gateway, joins GossipSub chat, reads/writes via stdin/stdout.
 * Usage:
 *   node peer.js --gateway /ip4/127.0.0.1/tcp/4001/p2p/<peerID> --nick alice
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
import { circuitRelayTransport } from '@libp2p/circuit-relay-v2'
import { multiaddr } from '@multiformats/multiaddr'
import { createInterface } from 'readline'
import { readFileSync, writeFileSync, existsSync } from 'fs'
import { generateKeyPair, privateKeyToProtobuf, privateKeyFromProtobuf } from '@libp2p/crypto/keys'
import process from 'process'

// ── CLI args ──────────────────────────────────────────────────────────────────

const args = parseArgs(process.argv.slice(2))
const gatewayAddr = args['gateway'] ?? ''
const topicName   = args['topic']   ?? 'libp2p_test-chat'
const keyFile     = args['keyfile'] ?? ''
let   nick        = args['nick']    ?? ''

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

// ── Key persistence ───────────────────────────────────────────────────────────

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

// ── Main ──────────────────────────────────────────────────────────────────────

async function main() {
  const privateKey = await loadOrCreateKey(keyFile)

  const hostOpts = {
    privateKey: keyFile ? privateKey : undefined,
    addresses: {
      listen: [
        '/ip4/0.0.0.0/tcp/0',
        '/ip4/0.0.0.0/tcp/0/ws',
      ],
    },
    transports: [
      tcp(),
      webSockets(),
      circuitRelayTransport({ discoverRelays: 1 }),
    ],
    connectionEncrypters: [noise()],
    streamMuxers: [yamux()],
    services: {
      identify: identify(),
      ping: ping(),
      dht: kadDHT({ mode: 'client' }),
      mdns: mdns({ serviceTag: 'libp2p_test' }),
      pubsub: gossipsub({
        allowPublishToZeroTopicPeers: true,
        emitSelf: false,
        multicodecs: ['/meshsub/1.1.0', '/meshsub/1.0.0'],
        scoreThresholds: {
          gossipThreshold: -Infinity,
          publishThreshold: -Infinity,
          graylistThreshold: -Infinity,
          acceptPXThreshold: 0,
          opportunisticGraftThreshold: 1,
        },
      }),
    },
  }

  const node = await createLibp2p(hostOpts)
  await node.start()

  const peerId = node.peerId
  if (!nick) nick = peerId.toString().slice(0, 8)

  console.log(`Peer ID:  ${peerId}`)
  console.log(`Nick:     ${nick}`)

  // Subscribe to chat topic before connecting so GossipSub sees the gateway
  // connection as new and opens a mesh stream immediately (same order as Go).
  node.services.pubsub.subscribe(topicName)
  node.services.pubsub.addEventListener('message', evt => {
    if (evt.detail.topic !== topicName) return
    try {
      const msg = JSON.parse(new TextDecoder().decode(evt.detail.data))
      process.stdout.write(`\r[${new Date().toLocaleTimeString()}] ${msg.sender_nick}: ${msg.body}\n> `)
    } catch { /* malformed */ }
  })

  // Connect to gateway if provided.
  if (gatewayAddr) {
    console.log(`Connecting to gateway…`)
    try {
      await node.dial(multiaddr(gatewayAddr))
      console.log('Connected to gateway.')
    } catch (err) {
      console.error(`Gateway connection failed: ${err.message}`)
      process.exit(1)
    }
  }

  console.log(`\nChat room : ${topicName}`)
  console.log(`You       : ${nick}`)
  console.log('Type a message and press Enter. Ctrl+C to quit.\n')

  // ── stdin read loop (mirrors bufio.Scanner in Go) ─────────────────────────

  const rl = createInterface({ input: process.stdin, output: process.stdout })
  process.stdout.write('> ')

  rl.on('line', async line => {
    const text = line.trim()
    if (!text) { process.stdout.write('> '); return }

    const msgData = new TextEncoder().encode(JSON.stringify({
      sender_id:   peerId.toString(),
      sender_nick: nick,
      body:        text,
      timestamp:   new Date().toISOString(),
    }))

    try {
      await node.services.pubsub.publish(topicName, msgData)
    } catch (err) {
      console.error(`publish: ${err.message}`)
    }
    process.stdout.write('> ')
  })

  // ── Graceful shutdown ────────────────────────────────────────────────────────

  const shutdown = async () => {
    console.log('\nBye!')
    rl.close()
    await node.stop()
    process.exit(0)
  }
  process.on('SIGINT',  shutdown)
  process.on('SIGTERM', shutdown)
}

main().catch(err => { console.error(err); process.exit(1) })
