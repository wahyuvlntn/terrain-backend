package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"afdd/internal/broker"
	"afdd/internal/datahub"
	"afdd/internal/httpx"
)

// acceptsCommands reports whether devices of a gateway type can be sent
// commands from here: REST push devices always (they pick commands up), MQTT
// devices when this process runs the embedded broker.
func (s *Server) acceptsCommands(gwType string) bool {
	return gwType == datahub.RESTServer || gwType == datahub.MQTTServer && s.Broker != nil
}

// sendCommand delivers a command to a device: published on its command topic
// of the embedded broker (MQTT_SERVER), or queued until the REST push device
// picks it up (GET /api/datahub/rest/{alias}/commands) or it expires.
func (s *Server) sendCommand(ctx context.Context, gateway, gwType, device string, payload []byte, expires time.Time) error {
	switch gwType {
	case datahub.RESTServer:
		return s.Hub.QueueCommand(ctx, gateway, device, payload, expires)
	case datahub.MQTTServer:
		if s.Broker == nil {
			return errors.New("the embedded MQTT broker does not run on this instance")
		}
		topic := broker.CommandTopic(s.Hub.MQTTIdentity(gateway), device)
		if device == "" {
			topic = strings.TrimSuffix(topic, "/")
		}
		return s.Broker.Publish(topic, payload)
	}
	return errors.New("the data source does not accept commands")
}

// serveRestCommands hands a REST push device its pending commands:
// GET /api/datahub/rest/{gateway_alias}/commands with the gateway's API key.
func (s *Server) serveRestCommands(w http.ResponseWriter, r *http.Request, alias string) {
	if r.Method != http.MethodGet {
		httpx.WriteError(w, r, &httpx.Error{Status: http.StatusMethodNotAllowed, Msg: "Method not allowed"})
		return
	}
	key := r.Header.Get("X-Api-Key")
	if key == "" {
		key = r.URL.Query().Get("api_key")
	}
	cmds, err := s.Hub.TakeCommands(r.Context(), alias, key)
	if err != nil {
		var he *httpx.Error
		if errors.As(err, &he) {
			httpx.WriteError(w, r, he)
			return
		}
		httpx.WriteError(w, r, httpx.Internal(err))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"commands": cmds})
}
