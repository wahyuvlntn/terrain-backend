package datahub

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gopcua/opcua"
	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/ua"
)

const opcuaAppURI = "urn:afdd:backend:opcua-client"

type opcuaDriver struct {
	mu     sync.Mutex
	client *opcua.Client
}

func opcuaEndpoint(g *Gateway) (string, error) {
	host := g.Param("IP_ADDRESS", "")
	if host == "" {
		return "", errors.New("IP_ADDRESS is required")
	}
	if strings.HasPrefix(host, "opc.tcp://") {
		return host, nil
	}
	ep := "opc.tcp://" + net.JoinHostPort(host, strconv.Itoa(g.ParamInt("PORT", 4840)))
	if name := strings.Trim(g.Param("SERVER_NAME", ""), "/ "); name != "" {
		ep += "/" + name
	}
	return ep, nil
}

func opcuaPolicy(s string) string {
	switch strings.ToLower(strings.ReplaceAll(s, " ", "")) {
	case "basic128rsa15":
		return "Basic128Rsa15"
	case "basic256":
		return "Basic256"
	case "basic256sha256":
		return "Basic256Sha256"
	default:
		return "None"
	}
}

func opcuaMode(s string) ua.MessageSecurityMode {
	switch strings.ToLower(strings.ReplaceAll(s, " ", "")) {
	case "sign":
		return ua.MessageSecurityModeSign
	case "signandencrypt":
		return ua.MessageSecurityModeSignAndEncrypt
	default:
		return ua.MessageSecurityModeNone
	}
}

// clientCert loads or creates the client certificate used for secured
// OPC UA channels.
func clientCert(dataDir string) ([]byte, *rsa.PrivateKey, error) {
	dir := filepath.Join(dataDir, "opcua")
	certPath := filepath.Join(dir, "client.der")
	keyPath := filepath.Join(dir, "client.key")
	if certDER, err := os.ReadFile(certPath); err == nil {
		if keyPEM, err := os.ReadFile(keyPath); err == nil {
			if blk, _ := pem.Decode(keyPEM); blk != nil {
				if key, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
					return certDER, key, nil
				}
			}
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	host, _ := os.Hostname()
	appURI, _ := url.Parse(opcuaAppURI)
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "AFDD OPC UA Client", Organization: []string{"AFDD"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageContentCommitment | x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageDataEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{host, "localhost"},
		URIs:                  []*url.URL{appURI},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(certPath, der, 0o644); err != nil {
		return nil, nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, nil, err
	}
	return der, key, nil
}

func (d *opcuaDriver) connect(ctx context.Context, rt *runtime) (*opcua.Client, error) {
	g := rt.gw
	endpoint, err := opcuaEndpoint(g)
	if err != nil {
		return nil, err
	}
	policy := opcuaPolicy(g.Param("POLICY", "None"))
	mode := opcuaMode(g.Param("ENCRYPTION", "None"))
	if policy == "None" {
		mode = ua.MessageSecurityModeNone
	} else if mode == ua.MessageSecurityModeNone {
		mode = ua.MessageSecurityModeSign
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	eps, err := opcua.GetEndpoints(cctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("get endpoints: %w", err)
	}
	ep, err := opcua.SelectEndpoint(eps, policy, mode)
	if err != nil {
		return nil, fmt.Errorf("select endpoint (%s/%s): %w", policy, mode, err)
	}
	anonymous := g.ParamBool("USE_ANONYMOUS_LOGIN", true)
	authType := ua.UserTokenTypeAnonymous
	if !anonymous {
		authType = ua.UserTokenTypeUserName
	}
	opts := []opcua.Option{
		opcua.SecurityFromEndpoint(ep, authType),
		opcua.ApplicationURI(opcuaAppURI),
		opcua.AutoReconnect(false),
		opcua.RequestTimeout(15 * time.Second),
		opcua.DialTimeout(10 * time.Second),
		opcua.SessionTimeout(30 * time.Minute),
	}
	if anonymous {
		opts = append(opts, opcua.AuthAnonymous())
	} else {
		opts = append(opts, opcua.AuthUsername(g.Param("USERNAME", ""), g.Param("PASSWORD", "")))
	}
	if policy != "None" || !anonymous {
		cert, key, err := clientCert(rt.hub.dataDir)
		if err != nil {
			return nil, fmt.Errorf("client certificate: %w", err)
		}
		opts = append(opts, opcua.Certificate(cert), opcua.PrivateKey(key))
	}
	c, err := opcua.NewClient(endpoint, opts...)
	if err != nil {
		return nil, err
	}
	if err := c.Connect(cctx); err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return c, nil
}

func variantString(v *ua.Variant) string {
	if v == nil {
		return ""
	}
	switch x := v.Value().(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float32:
		return formatFloat(float64(x))
	case float64:
		return formatFloat(x)
	case int8, int16, int32, int64, uint8, uint16, uint32, uint64:
		return fmt.Sprint(x)
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	case *ua.LocalizedText:
		if x == nil {
			return ""
		}
		return x.Text
	case *ua.QualifiedName:
		if x == nil {
			return ""
		}
		return x.Name
	case *ua.NodeID:
		return x.String()
	case []byte:
		return string(x)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprint(x)
		}
		return string(b)
	}
}

func (d *opcuaDriver) readNodes(ctx context.Context, c *opcua.Client, addresses []string) (map[string]VTQC, error) {
	out := map[string]VTQC{}
	type item struct {
		addr string
		id   *ua.NodeID
	}
	var items []item
	var firstErr error
	for _, a := range addresses {
		nid, err := ua.ParseNodeID(strings.TrimSpace(a))
		if err != nil {
			out[a] = VTQC{T: time.Now().UnixMilli(), Q: QualityConfig}
			if firstErr == nil {
				firstErr = fmt.Errorf("invalid node id %q", a)
			}
			continue
		}
		items = append(items, item{addr: a, id: nid})
	}
	const chunk = 500
	for i := 0; i < len(items); i += chunk {
		end := i + chunk
		if end > len(items) {
			end = len(items)
		}
		part := items[i:end]
		req := &ua.ReadRequest{MaxAge: 0, TimestampsToReturn: ua.TimestampsToReturnBoth}
		for _, it := range part {
			req.NodesToRead = append(req.NodesToRead, &ua.ReadValueID{NodeID: it.id, AttributeID: ua.AttributeIDValue})
		}
		rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		resp, err := c.Read(rctx, req)
		cancel()
		if err != nil {
			return out, err
		}
		now := time.Now().UnixMilli()
		for k, it := range part {
			if k >= len(resp.Results) {
				break
			}
			dv := resp.Results[k]
			v := VTQC{T: now, Q: QualityGood}
			if dv == nil || dv.Status != ua.StatusOK {
				v.Q = QualityBad
			}
			if dv != nil {
				v.V = variantString(dv.Value)
				if !dv.SourceTimestamp.IsZero() && dv.SourceTimestamp.Year() > 2000 {
					v.T = dv.SourceTimestamp.UnixMilli()
				} else if !dv.ServerTimestamp.IsZero() && dv.ServerTimestamp.Year() > 2000 {
					v.T = dv.ServerTimestamp.UnixMilli()
				}
			}
			out[it.addr] = v
		}
	}
	return out, firstErr
}

func (d *opcuaDriver) run(ctx context.Context, rt *runtime) {
	g := rt.gw
	defer d.closeClient()
	pollLoop(ctx, g.Seconds("INTERVAL", time.Second), func(ctx context.Context) error {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.client == nil {
			c, err := d.connect(ctx, rt)
			if err != nil {
				return err
			}
			d.client = c
		}
		addresses := make([]string, 0, len(g.Tags))
		for _, t := range g.Tags {
			addresses = append(addresses, t.Address)
		}
		res, err := d.readNodes(ctx, d.client, addresses)
		if err != nil && len(res) == 0 && len(addresses) > 0 {
			closeOPCUA(d.client)
			d.client = nil
			return err
		}
		values := map[string]VTQC{}
		for _, t := range g.Tags {
			if v, ok := res[t.Address]; ok {
				values[t.Name] = v
			}
		}
		rt.update(values)
		rt.setConnected(true, err)
		return nil
	}, func(err error) {
		rt.setConnected(false, err)
		rt.markAllBad()
	})
}

// closeOPCUA closes a client without waiting long for an unreachable server.
func closeOPCUA(c *opcua.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.Close(ctx)
}

func (d *opcuaDriver) closeClient() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.client != nil {
		closeOPCUA(d.client)
		d.client = nil
	}
}

// withClient runs fn with the live client or a temporary connection.
func (d *opcuaDriver) withClient(ctx context.Context, rt *runtime, fn func(c *opcua.Client) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.client != nil {
		return fn(d.client)
	}
	c, err := d.connect(ctx, rt)
	if err != nil {
		return err
	}
	defer closeOPCUA(c)
	return fn(c)
}

func (d *opcuaDriver) read(ctx context.Context, rt *runtime, addresses []string) (map[string]string, error) {
	out := map[string]string{}
	err := d.withClient(ctx, rt, func(c *opcua.Client) error {
		res, err := d.readNodes(ctx, c, addresses)
		for k, v := range res {
			out[k] = v.V
		}
		if len(res) == 0 {
			return err
		}
		return nil
	})
	return out, err
}

func (d *opcuaDriver) browse(ctx context.Context, rt *runtime, nodeID string, depth int) ([]*TagNode, error) {
	start := ua.NewNumericNodeID(0, id.ObjectsFolder)
	if strings.TrimSpace(nodeID) != "" {
		nid, err := ua.ParseNodeID(strings.TrimSpace(nodeID))
		if err != nil {
			return nil, fmt.Errorf("invalid node id %q", nodeID)
		}
		start = nid
	}
	if depth < 0 {
		depth = 1
	}
	var result []*TagNode
	err := d.withClient(ctx, rt, func(c *opcua.Client) error {
		budget := 5000
		var walk func(n *ua.NodeID, level int) ([]*TagNode, error)
		walk = func(n *ua.NodeID, level int) ([]*TagNode, error) {
			refs, err := c.Node(n).References(ctx, id.HierarchicalReferences, ua.BrowseDirectionForward, ua.NodeClassAll, true)
			if err != nil {
				return nil, err
			}
			var nodes []*TagNode
			for _, r := range refs {
				if budget <= 0 || ctx.Err() != nil {
					break
				}
				if r == nil || r.NodeID == nil || r.NodeID.NodeID == nil {
					continue
				}
				if r.NodeClass != ua.NodeClassObject && r.NodeClass != ua.NodeClassVariable {
					continue
				}
				budget--
				name := ""
				if r.DisplayName != nil {
					name = r.DisplayName.Text
				}
				if name == "" && r.BrowseName != nil {
					name = r.BrowseName.Name
				}
				tn := &TagNode{Address: r.NodeID.NodeID.String(), Name: name}
				if r.NodeClass == ua.NodeClassObject {
					if (depth == 0 && level < 12) || level+1 < depth {
						children, err := walk(r.NodeID.NodeID, level+1)
						if err == nil {
							tn.Children = children
						}
					}
					if tn.Children == nil {
						tn.Children = []*TagNode{}
					}
				}
				nodes = append(nodes, tn)
			}
			return nodes, nil
		}
		res, err := walk(start, 0)
		result = res
		return err
	})
	return result, err
}
