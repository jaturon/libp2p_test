import { createLibp2p } from 'libp2p'
import { webTransport } from '@libp2p/webtransport'
import { webSockets } from '@libp2p/websockets'
import { gossipsub } from '@chainsafe/libp2p-gossipsub'
import { noise } from '@chainsafe/libp2p-noise'
import { yamux } from '@libp2p/yamux'
import { identify } from '@libp2p/identify'
import { ping } from '@libp2p/ping'
import { multiaddr } from '@multiformats/multiaddr'

// ── state ────────────────────────────────────────────────────────────────────

let node = null
let nick = 'browser'
let topic = 'libp2p_test-chat'

// ── libp2p ───────────────────────────────────────────────────────────────────

async function connect(gatewayAddr) {
  log('Creating libp2p node…', 'info')

  node = await createLibp2p({
    transports: [
      webSockets({ filter: (addrs) => addrs }),  // allow all addrs (wss + ws)
      webTransport(),
    ],
    connectionEncrypters: [noise()],
    streamMuxers: [yamux()],
    // Allow dialing private/LAN IPs — blocked by default in libp2p v3.
    connectionGater: {
      denyDialMultiaddr: async () => false,
    },
    services: {
      // identify is required by gossipsub in libp2p v3.
      identify: identify(),
      ping: ping(),
      pubsub: gossipsub({
        allowPublishToZeroTopicPeers: true,
        emitSelf: false,
        // Limit to protocols the Go gateway supports (max /meshsub/1.1.0).
        multicodecs: ['/meshsub/1.1.0', '/meshsub/1.0.0'],
        // Relaxed scoring for a public demo: don't penalise browser peers.
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

  node.addEventListener('peer:connect', (evt) => {
    log(`Peer connected: ${shortId(evt.detail)}`, 'ok')
    refreshPeerCount()
  })
  node.addEventListener('peer:disconnect', (evt) => {
    log(`Peer disconnected: ${shortId(evt.detail)}`)
    refreshPeerCount()
  })

  await node.start()
  log(`Node started  ID: ${shortId(node.peerId)}`, 'ok')

  log('Dialing gateway…', 'info')
  try {
    await node.dial(multiaddr(gatewayAddr.trim()))
  } catch (err) {
    throw new Error(`Dial failed: ${err.message}`)
  }
  log('Connected to gateway!', 'ok')

  // Subscribe to the chat topic and listen for incoming messages.
  node.services.pubsub.subscribe(topic)
  node.services.pubsub.addEventListener('message', onPubsubMessage)
  log(`Subscribed to topic "${topic}"`, 'ok')

  // Keep the peer-count badge fresh.
  setInterval(refreshPeerCount, 5000)
}

function onPubsubMessage(evt) {
  if (evt.detail.topic !== topic) return
  let msg
  try {
    msg = JSON.parse(new TextDecoder().decode(evt.detail.data))
  } catch {
    return
  }
  appendMessage(msg.sender_nick ?? '(unknown)', msg.body ?? '', msg.timestamp, false)
}

async function publish(text) {
  if (!node) throw new Error('Not connected')
  const msg = {
    sender_id: node.peerId.toString(),
    sender_nick: nick,
    body: text,
    timestamp: new Date().toISOString(),
  }
  const data = new TextEncoder().encode(JSON.stringify(msg))
  await node.services.pubsub.publish(topic, data)
  // emitSelf is false — echo locally so the sender sees their own message.
  appendMessage(nick, text, msg.timestamp, true)
}

// ── UI helpers ────────────────────────────────────────────────────────────────

const $ = (id) => document.getElementById(id)

function log(msg, level = '') {
  const el = $('log')
  const line = document.createElement('div')
  line.className = 'log-line' + (level ? ' ' + level : '')
  line.textContent = `[${new Date().toLocaleTimeString()}] ${msg}`
  el.appendChild(line)
  el.scrollTop = el.scrollHeight
}

function appendMessage(senderNick, body, timestamp, isSelf) {
  const box = $('messages')
  const div = document.createElement('div')
  div.className = 'message' + (isSelf ? ' self' : '')
  const ts = timestamp ? new Date(timestamp).toLocaleTimeString() : ''
  div.innerHTML = `
    <div class="meta">
      <span class="nick">${esc(senderNick)}</span>
      <span class="ts">${ts}</span>
    </div>
    <p>${esc(body)}</p>`
  box.appendChild(div)
  box.scrollTop = box.scrollHeight
}

function setStatus(state) {
  const dot   = $('status-dot')
  const label = $('status-label')
  dot.className = ''
  if (state === 'connected') {
    dot.classList.add('connected')
    label.textContent = 'Connected'
  } else if (state === 'error') {
    dot.classList.add('error')
    label.textContent = 'Error'
  } else {
    label.textContent = 'Disconnected'
  }
}

function refreshPeerCount() {
  if (!node) return
  try {
    const subs = node.services.pubsub.getSubscribers(topic)
    $('peer-count').textContent = `${subs.length} peer${subs.length !== 1 ? 's' : ''} in topic`
  } catch { /* topic not joined yet */ }
}

function shortId(peerId) {
  const s = peerId.toString()
  return s.slice(0, 8) + '…' + s.slice(-4)
}

function esc(s) {
  return String(s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
}

// ── boot ──────────────────────────────────────────────────────────────────────

document.addEventListener('DOMContentLoaded', () => {
  // WebTransport is Chrome 97+ / Edge 97+ only.
  if (typeof WebTransport === 'undefined') {
    $('browser-warning').style.display = 'block'
    $('connect-btn').disabled = true
    log('WebTransport not available — use Chrome 97+ / Edge 97+', 'error')
  }

  const connectBtn = $('connect-btn')
  const sendBtn    = $('send-btn')
  const msgInput   = $('msg-input')

  // ── connect ──
  connectBtn.addEventListener('click', async () => {
    const addr     = $('addr-input').value.trim()
    const nickVal  = $('nick-input').value.trim()
    const topicVal = $('topic-input').value.trim()

    if (!addr) {
      log('Paste the gateway WebTransport multiaddr first.', 'error')
      return
    }

    if (nickVal)  nick  = nickVal
    if (topicVal) topic = topicVal

    connectBtn.disabled = true
    connectBtn.textContent = 'Connecting…'

    try {
      await connect(addr)
      setStatus('connected')
      connectBtn.textContent = 'Connected ✓'
      msgInput.disabled = false
      sendBtn.disabled  = false
      msgInput.focus()
      refreshPeerCount()
    } catch (err) {
      log(err.message, 'error')
      setStatus('error')
      connectBtn.disabled = false
      connectBtn.textContent = 'Retry'
    }
  })

  // ── send ──
  async function send() {
    const text = msgInput.value.trim()
    if (!text || !node) return
    msgInput.value = ''
    autoResize()
    try {
      await publish(text)
    } catch (err) {
      log('Send failed: ' + err.message, 'error')
    }
  }

  sendBtn.addEventListener('click', send)

  msgInput.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      send()
    }
  })

  // Auto-grow textarea up to ~6 lines.
  function autoResize() {
    msgInput.style.height = 'auto'
    msgInput.style.height = Math.min(msgInput.scrollHeight, 120) + 'px'
  }
  msgInput.addEventListener('input', autoResize)
})
