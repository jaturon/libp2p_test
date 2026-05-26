// Package chat implements a GossipSub-based chat room over libp2p.
package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"
)

const msgBufSize = 128

// Message is a chat message carried over GossipSub.
type Message struct {
	SenderID   string    `json:"sender_id"`
	SenderNick string    `json:"sender_nick"`
	Body       string    `json:"body"`
	Timestamp  time.Time `json:"timestamp"`
}

// Room wraps a GossipSub topic as a chat room.
// Incoming messages are delivered on the Messages channel.
type Room struct {
	ctx      context.Context
	topic    *pubsub.Topic
	sub      *pubsub.Subscription
	selfID   peer.ID
	nick     string
	Messages chan *Message
}

// JoinRoom joins (or creates) a GossipSub topic and returns a Room ready for use.
func JoinRoom(ctx context.Context, ps *pubsub.PubSub, selfID peer.ID, nick, topicName string) (*Room, error) {
	topic, err := ps.Join(topicName)
	if err != nil {
		return nil, fmt.Errorf("join topic %q: %w", topicName, err)
	}

	sub, err := topic.Subscribe()
	if err != nil {
		topic.Close()
		return nil, fmt.Errorf("subscribe %q: %w", topicName, err)
	}

	r := &Room{
		ctx:      ctx,
		topic:    topic,
		sub:      sub,
		selfID:   selfID,
		nick:     nick,
		Messages: make(chan *Message, msgBufSize),
	}
	go r.readLoop()
	return r, nil
}

// Publish sends a message to the chat room.
func (r *Room) Publish(body string) error {
	m := &Message{
		SenderID:   r.selfID.String(),
		SenderNick: r.nick,
		Body:       body,
		Timestamp:  time.Now(),
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return r.topic.Publish(r.ctx, data)
}

// Nick returns the local nickname.
func (r *Room) Nick() string { return r.nick }

// Peers lists the peer IDs currently subscribed to the same topic.
func (r *Room) Peers() []peer.ID { return r.topic.ListPeers() }

func (r *Room) readLoop() {
	for {
		msg, err := r.sub.Next(r.ctx)
		if err != nil {
			if r.ctx.Err() == nil {
				log.Printf("[chat] subscription closed: %v", err)
			}
			return
		}
		// Skip our own messages — pubsub echoes them back.
		if msg.ReceivedFrom == r.selfID {
			continue
		}
		m := new(Message)
		if err := json.Unmarshal(msg.Data, m); err != nil {
			continue
		}
		select {
		case r.Messages <- m:
		default: // drop if consumer is slow
		}
	}
}
