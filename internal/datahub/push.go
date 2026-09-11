package datahub

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// ---------------------------------------------------------------------------
// MQTT client: subscribes to topics on an external broker.
// ---------------------------------------------------------------------------

type mqttClientDriver struct{}

func splitTopics(s string) []string {
	var out []string
	for _, t := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == '\n' }) {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func (d *mqttClientDriver) run(ctx context.Context, rt *runtime) {
	g := rt.gw
	brokerURL := g.Param("URL", "")
	topics := splitTopics(g.Param("TOPIC", ""))
	if brokerURL == "" || len(topics) == 0 {
		rt.setConnected(false, errors.New("URL and TOPIC are required"))
		<-ctx.Done()
		return
	}
	// paho understands tcp/ssl/ws/wss/mqtt/mqtts schemes.
	opts := paho.NewClientOptions().AddBroker(brokerURL)
	clientID := g.Param("CLIENT_ID", "")
	if clientID == "" {
		clientID = "afdd-" + g.Alias + "-" + GeneratePassword()[:6]
	}
	opts.SetClientID(clientID)
	if g.ParamBool("USE_AUTHENTICATION", false) {
		opts.SetUsername(g.Param("USERNAME", ""))
		opts.SetPassword(g.Param("PASSWORD", ""))
	}
	opts.SetKeepAlive(g.Seconds("KEEP_ALIVE", 60*time.Second))
	opts.SetConnectTimeout(g.Seconds("CONNECT_TIMEOUT", 30*time.Second))
	opts.SetMaxReconnectInterval(g.Seconds("RECONNECT_PERIOD", 60*time.Second))
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(g.Seconds("RECONNECT_PERIOD", 10*time.Second))
	opts.SetCleanSession(true)
	opts.SetTLSConfig(&tls.Config{InsecureSkipVerify: true}) //nolint:gosec // field brokers commonly use self-signed certificates

	handler := func(_ paho.Client, m paho.Message) {
		values, err := rt.hub.mapPayload(g, m.Payload())
		if err != nil {
			rt.setConnected(true, err)
			return
		}
		// Prefix keys with the topic when several topics are subscribed.
		if len(topics) > 1 {
			prefixed := make(map[string]VTQC, len(values)*2)
			for k, v := range values {
				prefixed[k] = v
				prefixed[m.Topic()+"/"+k] = v
			}
			values = prefixed
		}
		rt.updateByAddress(values)
	}
	opts.SetOnConnectHandler(func(c paho.Client) {
		filters := map[string]byte{}
		for _, t := range topics {
			filters[t] = 0
		}
		tok := c.SubscribeMultiple(filters, handler)
		if tok.WaitTimeout(15*time.Second) && tok.Error() != nil {
			rt.setConnected(true, fmt.Errorf("subscribe: %w", tok.Error()))
			return
		}
		rt.setConnected(true, nil)
	})
	opts.SetConnectionLostHandler(func(_ paho.Client, err error) {
		rt.setConnected(false, err)
		rt.markAllBad()
	})
	client := paho.NewClient(opts)
	tok := client.Connect()
	go func() {
		tok.Wait()
		if err := tok.Error(); err != nil {
			rt.setConnected(false, err)
		}
	}()
	<-ctx.Done()
	client.Disconnect(500)
}

func (d *mqttClientDriver) read(_ context.Context, rt *runtime, addresses []string) (map[string]string, error) {
	return pushRead(rt, addresses), nil
}

func (d *mqttClientDriver) browse(_ context.Context, rt *runtime, _ string, _ int) ([]*TagNode, error) {
	return pushBrowse(rt), nil
}

// ---------------------------------------------------------------------------
// MQTT server: devices publish to the embedded broker.
// ---------------------------------------------------------------------------

type mqttServerDriver struct{}

func (d *mqttServerDriver) run(ctx context.Context, rt *runtime) {
	h := rt.hub
	g := rt.gw
	if h.broker == nil {
		rt.setConnected(false, errors.New("embedded MQTT broker is disabled"))
		<-ctx.Done()
		return
	}
	password := g.Secrets["mqtt_password"]
	if password == "" {
		rt.setConnected(false, errors.New("MQTT password not generated"))
		<-ctx.Done()
		return
	}
	identity := h.MQTTIdentity(g.Alias)
	topics := []string{identity, identity + "/#"}
	err := h.broker.SetGateway(identity, password, topics, func(topic string, payload []byte) {
		values, err := h.mapPayload(g, payload)
		if err != nil {
			rt.setConnected(true, err)
			return
		}
		if topic != identity {
			sub := strings.TrimPrefix(topic, identity+"/")
			// Values of sub topics are addressed as "<sub topic>/<key>" so
			// they never overwrite values of the main topic.
			prefixed := make(map[string]VTQC, len(values))
			for k, v := range values {
				prefixed[sub+"/"+k] = v
			}
			values = prefixed
		}
		rt.updateByAddress(values)
	})
	if err != nil {
		rt.setConnected(false, err)
		<-ctx.Done()
		return
	}
	// Connected = at least one device session with the gateway username.
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		rt.setConnected(h.broker.HasClientsFor(identity), nil)
		select {
		case <-ctx.Done():
			h.broker.RemoveGateway(identity)
			return
		case <-t.C:
		}
	}
}

func (d *mqttServerDriver) read(_ context.Context, rt *runtime, addresses []string) (map[string]string, error) {
	return pushRead(rt, addresses), nil
}

func (d *mqttServerDriver) browse(_ context.Context, rt *runtime, _ string, _ int) ([]*TagNode, error) {
	return pushBrowse(rt), nil
}

// ---------------------------------------------------------------------------
// REST server: devices POST JSON to /api/datahub/rest/{alias}.
// ---------------------------------------------------------------------------

type restServerDriver struct{}

func (d *restServerDriver) run(ctx context.Context, rt *runtime) {
	rt.setConnected(true, nil)
	if rt.hub.mode != ModeWorker {
		<-ctx.Done()
		return
	}
	// worker: the API replicas receive the pushes and write them to the
	// shared state; read them back from there
	var since int64
	pollLoop(ctx, rt.hub.refreshEvery, func(ctx context.Context) error {
		var err error
		since, err = rt.hub.pullShared(ctx, rt, since)
		return err
	}, func(err error) { slog.Warn("read REST push values", "alias", rt.gw.Alias, "err", err) })
}

func (d *restServerDriver) read(_ context.Context, rt *runtime, addresses []string) (map[string]string, error) {
	return pushRead(rt, addresses), nil
}

func (d *restServerDriver) browse(_ context.Context, rt *runtime, _ string, _ int) ([]*TagNode, error) {
	return pushBrowse(rt), nil
}

// ---------------------------------------------------------------------------
// REST client: polls an HTTP endpoint.
// ---------------------------------------------------------------------------

type restClientDriver struct{}

type keyValue struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func parseKV(s string) []keyValue {
	var kv []keyValue
	if strings.TrimSpace(s) == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(s), &kv); err != nil {
		return nil
	}
	out := kv[:0]
	for _, x := range kv {
		if strings.TrimSpace(x.Key) != "" {
			out = append(out, x)
		}
	}
	return out
}

var insecureHTTP = &http.Client{
	Transport: &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // field devices commonly use self-signed certificates
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	},
}

func (d *restClientDriver) fetch(ctx context.Context, g *Gateway) ([]byte, error) {
	rawURL := g.Param("URL", "")
	if rawURL == "" {
		return nil, errors.New("URL is required")
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("err_url_http_https")
	}
	q := u.Query()
	for _, p := range parseKV(g.Param("PARAM", "[]")) {
		q.Add(p.Key, p.Value)
	}
	u.RawQuery = q.Encode()
	method := strings.ToUpper(g.Param("METHOD", http.MethodGet))
	var body io.Reader
	if method == http.MethodPost {
		obj := map[string]string{}
		for _, b := range parseKV(g.Param("BODY", "[]")) {
			obj[b.Key] = b.Value
		}
		buf, _ := json.Marshal(obj)
		body = bytes.NewReader(buf)
	} else {
		method = http.MethodGet
	}
	tctx, cancel := context.WithTimeout(ctx, g.Seconds("TIMEOUT", 10*time.Second))
	defer cancel()
	req, err := http.NewRequestWithContext(tctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	for _, h := range parseKV(g.Param("HEADER", "[]")) {
		req.Header.Set(h.Key, h.Value)
	}
	if g.ParamBool("BASIC_AUTHENTICATION", false) {
		req.SetBasicAuth(g.Param("USERNAME", ""), g.Param("PASSWORD", ""))
	}
	resp, err := insecureHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("http status %d", resp.StatusCode)
	}
	return data, nil
}

func (d *restClientDriver) run(ctx context.Context, rt *runtime) {
	g := rt.gw
	pollLoop(ctx, g.Seconds("INTERVAL", 5*time.Second), func(ctx context.Context) error {
		data, err := d.fetch(ctx, g)
		if err != nil {
			return err
		}
		values, err := rt.hub.mapPayload(g, data)
		if err != nil {
			rt.setConnected(true, err)
			return nil
		}
		rt.setConnected(true, nil)
		rt.updateByAddress(values)
		return nil
	}, func(err error) {
		slog.Debug("rest client poll failed", "gateway", g.Alias, "err", err)
		rt.setConnected(false, err)
		rt.markAllBad()
	})
}

func (d *restClientDriver) read(ctx context.Context, rt *runtime, addresses []string) (map[string]string, error) {
	data, err := d.fetch(ctx, rt.gw)
	if err != nil {
		return pushRead(rt, addresses), nil
	}
	values, err := rt.hub.mapPayload(rt.gw, data)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, a := range addresses {
		if v, ok := values[a]; ok {
			out[a] = v.V
		}
	}
	return out, nil
}

func (d *restClientDriver) browse(ctx context.Context, rt *runtime, _ string, _ int) ([]*TagNode, error) {
	data, err := d.fetch(ctx, rt.gw)
	if err != nil {
		return pushBrowse(rt), nil
	}
	values, err := rt.hub.mapPayload(rt.gw, data)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	rt.mu.Lock()
	rt.keys = mergeKeys(rt.keys, keys)
	rt.mu.Unlock()
	return pushBrowse(rt), nil
}
