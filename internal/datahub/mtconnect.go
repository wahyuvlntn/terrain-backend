package datahub

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MTConnect address: <dataItemId>.[tag | value | _attribute.<attribute>]
type mtconnectDriver struct{}

type mtItem struct {
	tag   string
	value string
	attrs map[string]string
}

func (d *mtconnectDriver) fetch(ctx context.Context, g *Gateway) (map[string]*mtItem, error) {
	host := g.Param("HOST", "")
	if host == "" {
		return nil, errors.New("HOST is required")
	}
	base := host
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "http://" + net.JoinHostPort(host, strconv.Itoa(g.ParamInt("PORT", 5000)))
	}
	u := strings.TrimRight(base, "/")
	if dev := g.Param("DEVICE_NAME", ""); dev != "" {
		u += "/" + url.PathEscape(dev)
	}
	u += "/current"
	tctx, cancel := context.WithTimeout(ctx, g.Seconds("TIMEOUT", 10*time.Second))
	defer cancel()
	req, err := http.NewRequestWithContext(tctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := insecureHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("http status %d", resp.StatusCode)
	}
	return parseMTConnect(io.LimitReader(resp.Body, 20<<20))
}

func parseMTConnect(r io.Reader) (map[string]*mtItem, error) {
	dec := xml.NewDecoder(r)
	items := map[string]*mtItem{}
	var cur *mtItem
	var curID string
	depth := 0
	var text strings.Builder
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if cur != nil {
				depth++
				continue
			}
			id := ""
			attrs := map[string]string{}
			for _, a := range t.Attr {
				attrs[a.Name.Local] = a.Value
				if a.Name.Local == "dataItemId" {
					id = a.Value
				}
			}
			if id != "" {
				cur = &mtItem{tag: t.Name.Local, attrs: attrs}
				curID = id
				depth = 0
				text.Reset()
			}
		case xml.CharData:
			if cur != nil {
				text.Write(t)
			}
		case xml.EndElement:
			if cur != nil {
				if depth > 0 {
					depth--
					continue
				}
				cur.value = strings.TrimSpace(text.String())
				items[curID] = cur
				cur = nil
			}
		}
	}
	if len(items) == 0 {
		return nil, errors.New("no MTConnect data items found")
	}
	return items, nil
}

func mtValue(items map[string]*mtItem, address string) (string, bool) {
	id, field, ok := strings.Cut(address, ".")
	if !ok {
		field = "value"
	}
	it, ok := items[id]
	if !ok {
		return "", false
	}
	switch {
	case field == "value":
		return it.value, true
	case field == "tag":
		return it.tag, true
	case strings.HasPrefix(field, "_attribute."):
		v, ok := it.attrs[strings.TrimPrefix(field, "_attribute.")]
		return v, ok
	}
	return "", false
}

func (d *mtconnectDriver) run(ctx context.Context, rt *runtime) {
	g := rt.gw
	pollLoop(ctx, g.Seconds("INTERVAL", time.Second), func(ctx context.Context) error {
		items, err := d.fetch(ctx, g)
		if err != nil {
			return err
		}
		now := time.Now().UnixMilli()
		values := map[string]VTQC{}
		for _, t := range g.Tags {
			v, ok := mtValue(items, t.Address)
			q := QualityGood
			if !ok || strings.EqualFold(v, "UNAVAILABLE") {
				q = QualityBad
			}
			values[t.Name] = VTQC{V: v, T: now, Q: q}
		}
		rt.update(values)
		rt.setConnected(true, nil)
		return nil
	}, func(err error) {
		rt.setConnected(false, err)
		rt.markAllBad()
	})
}

func (d *mtconnectDriver) read(ctx context.Context, rt *runtime, addresses []string) (map[string]string, error) {
	items, err := d.fetch(ctx, rt.gw)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, a := range addresses {
		if v, ok := mtValue(items, a); ok {
			out[a] = v
		}
	}
	return out, nil
}

func (d *mtconnectDriver) browse(ctx context.Context, rt *runtime, _ string, _ int) ([]*TagNode, error) {
	items, err := d.fetch(ctx, rt.gw)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(items))
	for id := range items {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*TagNode, 0, len(ids))
	for _, id := range ids {
		it := items[id]
		n := &TagNode{Address: id, Name: id + " (" + it.tag + ")"}
		n.Children = append(n.Children, &TagNode{Address: id + ".value", Name: "value"}, &TagNode{Address: id + ".tag", Name: "tag"})
		keys := make([]string, 0, len(it.attrs))
		for k := range it.attrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			n.Children = append(n.Children, &TagNode{Address: id + "._attribute." + k, Name: "_attribute." + k})
		}
		out = append(out, n)
	}
	return out, nil
}
