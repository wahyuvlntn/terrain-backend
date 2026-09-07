package integration

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	opcid "github.com/gopcua/opcua/id"
	opcserver "github.com/gopcua/opcua/server"
	"github.com/gopcua/opcua/ua"
	mochi "github.com/mochi-mqtt/server/v2"
	mochiauth "github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/simonvetter/modbus"

	"afdd/internal/broker"
	"afdd/internal/datahub"
)

// gateway inserts a data source with tags directly in the database.
func gateway(t *testing.T, alias, typ string, params map[string]string, secrets map[string]string, tags map[string]string) {
	t.Helper()
	p, _ := json.Marshal(params)
	if secrets == nil {
		secrets = map[string]string{}
	}
	s, _ := json.Marshal(secrets)
	var gid int64
	if err := E.db.QueryRow(E.ctx, `INSERT INTO dh.gateways (alias, name, type, params, secrets, is_active) VALUES ($1,$1,$2,$3,$4,TRUE) RETURNING id`,
		alias, typ, p, s).Scan(&gid); err != nil {
		t.Fatal(err)
	}
	for name, addr := range tags {
		E.exec(t, `INSERT INTO dh.gateway_tags (gateway_id, name, address) VALUES ($1,$2,$3)`, gid, name, addr)
	}
}

func waitValue(t *testing.T, h *datahub.Hub, alias, tag, want string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last datahub.VTQC
	for time.Now().Before(deadline) {
		if v, ok := h.Value(alias, tag); ok {
			last = v
			if v.V == want && v.Q == 0 {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s/%s: want %q, last %+v, status %+v", alias, tag, want, last, h.Status(alias))
}

type modbusHandler struct{ holding []uint16 }

func (h *modbusHandler) HandleCoils(req *modbus.CoilsRequest) ([]bool, error) {
	out := make([]bool, req.Quantity)
	for i := range out {
		out[i] = (int(req.Addr)+i)%2 == 0
	}
	return out, nil
}
func (h *modbusHandler) HandleDiscreteInputs(req *modbus.DiscreteInputsRequest) ([]bool, error) {
	return make([]bool, req.Quantity), nil
}
func (h *modbusHandler) HandleHoldingRegisters(req *modbus.HoldingRegistersRequest) ([]uint16, error) {
	if int(req.Addr)+int(req.Quantity) > len(h.holding) {
		return nil, modbus.ErrIllegalDataAddress
	}
	return h.holding[req.Addr : req.Addr+req.Quantity], nil
}
func (h *modbusHandler) HandleInputRegisters(req *modbus.InputRegistersRequest) ([]uint16, error) {
	out := make([]uint16, req.Quantity)
	for i := range out {
		out[i] = uint16(1000 + int(req.Addr) + i)
	}
	return out, nil
}

func TestDataHubProtocols(t *testing.T) {
	ctx, cancel := context.WithCancel(E.ctx)
	defer cancel()

	// Embedded broker (as in production) for MQTT_SERVER data sources.
	br := broker.New(broker.Options{TCPAddr: "127.0.0.1:21884", UIUsername: "ui", UIPassword: "ui-pass", NotificationTopic: "AFDD_NOTIFICATION"})
	if err := br.Start(); err != nil {
		t.Fatal(err)
	}
	defer br.Close()
	hub := datahub.New(E.db, br, 2*time.Second, t.TempDir(), "local.digital_twin_afdd")
	defer hub.Stop()

	t.Run("mqtt server", func(t *testing.T) {
		gateway(t, "dh_mqtt_srv", datahub.MQTTServer, map[string]string{}, map[string]string{"mqtt_password": "dev-secret"},
			map[string]string{"temp": "temp", "nested": "line.flow", "boiler": "boiler/temp"})
		if err := hub.Reload(ctx, "dh_mqtt_srv"); err != nil {
			t.Fatal(err)
		}
		identity := hub.MQTTIdentity("dh_mqtt_srv")
		connect := func(user, pass string) (paho.Client, error) {
			c := paho.NewClient(paho.NewClientOptions().AddBroker("tcp://127.0.0.1:21884").SetUsername(user).SetPassword(pass).
				SetClientID(user + "-" + time.Now().Format("150405.000")).SetConnectTimeout(5 * time.Second))
			tok := c.Connect()
			tok.Wait()
			return c, tok.Error()
		}
		if _, err := connect(identity, "wrong"); err == nil {
			t.Fatal("device with a wrong password must be rejected")
		}
		dev, err := connect(identity, "dev-secret")
		if err != nil {
			t.Fatal(err)
		}
		defer dev.Disconnect(100)
		dev.Publish(identity, 1, false, `{"temp": 42.5, "line": {"flow": 7}}`).Wait()
		waitValue(t, hub, "dh_mqtt_srv", "temp", "42.5")
		waitValue(t, hub, "dh_mqtt_srv", "nested", "7")
		// Sub topics are addressed as <sub topic>/<key>.
		dev.Publish(identity+"/boiler", 1, false, `{"temp": 81}`).Wait()
		waitValue(t, hub, "dh_mqtt_srv", "boiler", "81")
		// A device cannot publish into another data source.
		dev.Publish("local.digital_twin_afdd.other", 1, false, `{"temp": 1}`).Wait()
		// The UI account may only read notifications.
		ui, err := connect("ui", "ui-pass")
		if err != nil {
			t.Fatal(err)
		}
		defer ui.Disconnect(100)
		ui.Publish(identity, 1, false, `{"temp": 1}`).Wait()
		time.Sleep(300 * time.Millisecond)
		if v, _ := hub.Value("dh_mqtt_srv", "temp"); v.V != "42.5" {
			t.Fatalf("UI account could inject data: %+v", v)
		}
		// (The broker disconnects a v3 client after an unauthorized QoS 1
		// publish, so use a new connection for the subscription.)
		ui2, err := connect("ui", "ui-pass")
		if err != nil {
			t.Fatal(err)
		}
		defer ui2.Disconnect(100)
		got := make(chan string, 1)
		ui2.Subscribe("AFDD_NOTIFICATION", 0, func(_ paho.Client, m paho.Message) { got <- string(m.Payload()) }).Wait()
		time.Sleep(200 * time.Millisecond)
		if err := br.Publish("AFDD_NOTIFICATION", []byte(`[{"id":1}]`)); err != nil {
			t.Fatal(err)
		}
		select {
		case <-got:
		case <-time.After(5 * time.Second):
			t.Fatal("UI did not receive the notification")
		}
	})

	t.Run("mqtt client", func(t *testing.T) {
		// An external broker the data source subscribes to.
		ext := mochi.New(&mochi.Options{InlineClient: true})
		_ = ext.AddHook(new(mochiauth.AllowHook), nil)
		if err := ext.AddListener(listeners.NewTCP(listeners.Config{ID: "ext", Address: "127.0.0.1:21885"})); err != nil {
			t.Fatal(err)
		}
		go func() { _ = ext.Serve() }()
		defer ext.Close()
		gateway(t, "dh_mqtt_cli", datahub.MQTTClient, map[string]string{"URL": "tcp://127.0.0.1:21885", "TOPIC": "plant/line1",
			"CUSTOM_MAPPING": "true", "EXPRESSION": "output = { pressure: response.p / 10 }"}, nil, map[string]string{"pressure": "pressure"})
		if err := hub.Reload(ctx, "dh_mqtt_cli"); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for !hub.Status("dh_mqtt_cli").Connected && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		_ = ext.Publish("plant/line1", []byte(`{"p": 355}`), false, 0)
		waitValue(t, hub, "dh_mqtt_cli", "pressure", "35.5")
	})

	t.Run("rest client", func(t *testing.T) {
		calls := 0
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if u, p, ok := r.BasicAuth(); !ok || u != "api" || p != "secret" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"data": {"flow": 12.5, "ok": true}}`))
		}))
		defer api.Close()
		gateway(t, "dh_rest", datahub.RESTClient, map[string]string{"URL": api.URL, "METHOD": "GET", "INTERVAL": "1",
			"BASIC_AUTHENTICATION": "true", "USERNAME": "api", "PASSWORD": "secret"},
			nil, map[string]string{"flow": "data.flow", "ok": "data.ok"})
		if err := hub.Reload(ctx, "dh_rest"); err != nil {
			t.Fatal(err)
		}
		waitValue(t, hub, "dh_rest", "flow", "12.5")
		waitValue(t, hub, "dh_rest", "ok", "true")
	})

	t.Run("modbus tcp", func(t *testing.T) {
		regs := make([]uint16, 20)
		regs[0] = 1234 // 40001 INT
		signed := int16(-5)
		regs[1] = uint16(signed)       // 40002 INT (signed)
		bits := math.Float32bits(21.5) // 40003-40004 REAL (big endian words)
		regs[2], regs[3] = uint16(bits>>16), uint16(bits)
		srv, err := modbus.NewServer(&modbus.ServerConfiguration{URL: "tcp://127.0.0.1:21502", Timeout: 30 * time.Second, MaxClients: 5},
			&modbusHandler{holding: regs})
		if err != nil {
			t.Fatal(err)
		}
		if err := srv.Start(); err != nil {
			t.Fatal(err)
		}
		defer srv.Stop()
		gateway(t, "dh_modbus", datahub.ModbusTCP, map[string]string{"IP_ADDRESS": "127.0.0.1", "PORT": "21502", "INTERVAL": "1"}, nil,
			map[string]string{"count": "40001", "signed": "40002:INT", "temp": "40003:REAL", "coil": "00001", "input": "30005:UINT"})
		if err := hub.Reload(ctx, "dh_modbus"); err != nil {
			t.Fatal(err)
		}
		waitValue(t, hub, "dh_modbus", "count", "1234")
		waitValue(t, hub, "dh_modbus", "signed", "-5")
		waitValue(t, hub, "dh_modbus", "temp", "21.5")
		waitValue(t, hub, "dh_modbus", "coil", "true")
		waitValue(t, hub, "dh_modbus", "input", "1004")
		// On-demand read (used by the tag editor).
		vals, err := hub.Read(ctx, "dh_modbus", []string{"40001", "40003:REAL"})
		if err != nil || vals["40001"] != "1234" || vals["40003:REAL"] != "21.5" {
			t.Fatalf("on demand read: %v %v", vals, err)
		}
	})

	t.Run("opc ua", func(t *testing.T) {
		s := opcserver.New(opcserver.EnableSecurity("None", ua.MessageSecurityModeNone), opcserver.EnableAuthMode(ua.UserTokenTypeAnonymous),
			opcserver.EndPoint("127.0.0.1", 24840))
		ns := opcserver.NewMapNamespace(s, "Plant")
		ns.Data["Temperature"] = 23.25
		ns.Data["Running"] = true
		root, _ := s.Namespace(0)
		root.Objects().AddRef(ns.Objects(), opcid.HasComponent, true)
		if err := s.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		nsID := ns.ID()
		addr := func(k string) string { return "ns=" + itoa(int(nsID)) + ";s=" + k }
		gateway(t, "dh_opcua", datahub.OPCUA, map[string]string{"IP_ADDRESS": "127.0.0.1", "PORT": "24840", "INTERVAL": "1",
			"POLICY": "None", "ENCRYPTION": "None", "USE_ANONYMOUS_LOGIN": "true"}, nil,
			map[string]string{"temp": addr("Temperature"), "running": addr("Running")})
		if err := hub.Reload(ctx, "dh_opcua"); err != nil {
			t.Fatal(err)
		}
		t0 := time.Now()
		waitValue(t, hub, "dh_opcua", "temp", "23.25")
		t.Logf("first value after %s", time.Since(t0))
		waitValue(t, hub, "dh_opcua", "running", "true")
		ns.SetValue("Temperature", 24.5)
		waitValue(t, hub, "dh_opcua", "temp", "24.5")
		t1 := time.Now()
		nodes, err := hub.Browse(ctx, "dh_opcua", "", 2)
		t.Logf("browse took %s (%d nodes)", time.Since(t1), len(nodes))
		if err != nil || len(nodes) == 0 {
			t.Fatalf("browse: %v %v", nodes, err)
		}
	})
}
