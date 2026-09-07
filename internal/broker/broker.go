// Package broker embeds an MQTT broker. It serves two purposes:
//   - push notifications to the web UI (websocket listener, topic
//     AFDD_NOTIFICATION by default), and
//   - receive data from field devices for MQTT_SERVER gateways (TCP listener).
package broker

import (
	"bytes"
	"crypto/subtle"
	"crypto/tls"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"
)

// Options configure the broker.
type Options struct {
	TCPAddr           string
	TLSAddr           string
	TLSConfig         *tls.Config
	WSAddr            string
	WSTLS             *tls.Config
	UIUsername        string
	UIPassword        string
	NotificationTopic string
}

// Broker is the embedded MQTT server.
type Broker struct {
	srv  *mqtt.Server
	opts Options

	mu       sync.RWMutex
	gateways map[string]gatewayCred // username -> credentials
	subID    int
	subs     map[string][]subscription // gateway alias -> inline subscriptions
}

type gatewayCred struct {
	password string
	topics   []string
}

type subscription struct {
	filter string
	id     int
}

// MessageHandler receives messages published by devices.
type MessageHandler func(topic string, payload []byte)

// New creates (but does not start) the broker.
func New(o Options) *Broker {
	b := &Broker{opts: o, gateways: map[string]gatewayCred{}, subs: map[string][]subscription{}, subID: 100}
	logger := slog.Default().With("component", "mqtt")
	b.srv = mqtt.New(&mqtt.Options{InlineClient: true, Logger: logger})
	_ = b.srv.AddHook(&authHook{b: b}, nil)
	return b
}

// Start opens the listeners and starts serving.
func (b *Broker) Start() error {
	if b.opts.TCPAddr != "" {
		if err := b.srv.AddListener(listeners.NewTCP(listeners.Config{ID: "tcp", Address: b.opts.TCPAddr})); err != nil {
			return fmt.Errorf("mqtt tcp listener: %w", err)
		}
	}
	if b.opts.TLSAddr != "" && b.opts.TLSConfig != nil {
		if err := b.srv.AddListener(listeners.NewTCP(listeners.Config{ID: "tls", Address: b.opts.TLSAddr, TLSConfig: b.opts.TLSConfig})); err != nil {
			return fmt.Errorf("mqtt tls listener: %w", err)
		}
	}
	if b.opts.WSAddr != "" {
		if err := b.srv.AddListener(listeners.NewWebsocket(listeners.Config{ID: "ws", Address: b.opts.WSAddr, TLSConfig: b.opts.WSTLS})); err != nil {
			return fmt.Errorf("mqtt websocket listener: %w", err)
		}
	}
	go func() {
		if err := b.srv.Serve(); err != nil {
			slog.Error("mqtt broker stopped", "err", err)
		}
	}()
	slog.Info("mqtt broker started", "tcp", b.opts.TCPAddr, "tls", b.opts.TLSAddr, "ws", b.opts.WSAddr, "ws_tls", b.opts.WSTLS != nil)
	return nil
}

// Close stops the broker.
func (b *Broker) Close() error { return b.srv.Close() }

// CommandTopic is the topic of commands (set point and mode writes) for one
// device of an MQTT_SERVER gateway: "<gateway identity>/cmd/<device>". The
// gateway's devices may subscribe to their own "<identity>/cmd/#".
func CommandTopic(identity, device string) string { return identity + "/cmd/" + device }

// Publish sends a message to every subscriber.
func (b *Broker) Publish(topic string, payload []byte) error {
	if b == nil {
		return nil
	}
	return b.srv.Publish(topic, payload, false, 0)
}

// SetGateway registers MQTT credentials for an MQTT_SERVER gateway and
// subscribes the handler to the gateway topics.
func (b *Broker) SetGateway(alias, password string, topics []string, handler MessageHandler) error {
	if b == nil {
		return nil
	}
	b.RemoveGateway(alias)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gateways[alias] = gatewayCred{password: password, topics: topics}
	for _, t := range topics {
		b.subID++
		id := b.subID
		h := handler
		if err := b.srv.Subscribe(t, id, func(_ *mqtt.Client, _ packets.Subscription, pk packets.Packet) {
			// Inline handlers always receive the inline client; the
			// publisher is in pk.Origin. Ignore our own publications.
			if pk.Origin == mqtt.InlineClientId {
				return
			}
			h(pk.TopicName, append([]byte(nil), pk.Payload...))
		}); err != nil {
			return fmt.Errorf("subscribe %s: %w", t, err)
		}
		b.subs[alias] = append(b.subs[alias], subscription{filter: t, id: id})
	}
	return nil
}

// RemoveGateway removes credentials and subscriptions of a gateway.
func (b *Broker) RemoveGateway(alias string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.gateways, alias)
	for _, s := range b.subs[alias] {
		_ = b.srv.Unsubscribe(s.filter, s.id)
	}
	delete(b.subs, alias)
}

// HasClientsFor reports whether any non-inline client is connected with the
// given username (used to show MQTT_SERVER gateway connection state).
func (b *Broker) HasClientsFor(username string) bool {
	if b == nil {
		return false
	}
	for _, cl := range b.srv.Clients.GetAll() {
		if cl.ID == mqtt.InlineClientId || cl.Closed() {
			continue
		}
		if string(cl.Properties.Username) == username {
			return true
		}
	}
	return false
}

type authHook struct {
	mqtt.HookBase
	b *Broker
}

func (h *authHook) ID() string { return "afdd-auth" }

func (h *authHook) Provides(b byte) bool {
	return bytes.Contains([]byte{mqtt.OnConnectAuthenticate, mqtt.OnACLCheck}, []byte{b})
}

func eq(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func (h *authHook) OnConnectAuthenticate(cl *mqtt.Client, pk packets.Packet) bool {
	user := string(pk.Connect.Username)
	pass := string(pk.Connect.Password)
	if h.b.opts.UIUsername != "" && eq(user, h.b.opts.UIUsername) && eq(pass, h.b.opts.UIPassword) {
		return true
	}
	h.b.mu.RLock()
	defer h.b.mu.RUnlock()
	if c, ok := h.b.gateways[user]; ok && c.password != "" && eq(pass, c.password) {
		return true
	}
	return false
}

func (h *authHook) OnACLCheck(cl *mqtt.Client, topic string, write bool) bool {
	if cl == nil || cl.ID == mqtt.InlineClientId {
		return true
	}
	user := string(cl.Properties.Username)
	if h.b.opts.UIUsername != "" && user == h.b.opts.UIUsername {
		// The web UI may only read notifications.
		return !write && matchTopic(h.b.opts.NotificationTopic, topic)
	}
	h.b.mu.RLock()
	defer h.b.mu.RUnlock()
	if c, ok := h.b.gateways[user]; ok {
		// A device reads only the commands addressed to its gateway and never
		// publishes into that command subtree.
		cmd := CommandTopic(user, "#")
		if !write {
			return matchTopic(cmd, topic)
		}
		if matchTopic(cmd, topic) {
			return false
		}
		for _, f := range c.topics {
			if matchTopic(f, topic) {
				return true
			}
		}
	}
	return false
}

// matchTopic reports whether topic matches an MQTT filter (+ and # wildcards).
func matchTopic(filter, topic string) bool {
	if filter == topic {
		return true
	}
	fp := strings.Split(filter, "/")
	tp := strings.Split(topic, "/")
	for i, f := range fp {
		if f == "#" {
			return true
		}
		if i >= len(tp) {
			return false
		}
		if f != "+" && f != tp[i] {
			return false
		}
	}
	return len(fp) == len(tp)
}
