package datahub

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robinson/gos7"
)

// S7 address: <area address>:<DATATYPE>, e.g. DB1.DBX0.1:BOOL, DB1.DBW2:INT,
// DB10.DBD4:REAL, MW10:INT, M0.1:BOOL, I0.0:BOOL, QB1:USINT.
var (
	s7DBRe   = regexp.MustCompile(`^DB(\d+)\.(?:DB([XBWD]))?(\d+)(?:\.(\d+))?$`)
	s7AreaRe = regexp.MustCompile(`^([MIEQA])([XBWD]?)(\d+)(?:\.(\d+))?$`)
)

type s7Tag struct {
	raw      string
	area     byte // 'D' data block, 'M' merker, 'I' inputs, 'Q' outputs
	db       int
	offset   int
	bit      int
	dataType string
}

func (t *s7Tag) size() int {
	switch t.dataType {
	case "BOOL", "SINT", "USINT", "BYTE", "CHAR":
		return 1
	case "INT", "UINT", "WORD":
		return 2
	case "DINT", "UDINT", "REAL", "DWORD":
		return 4
	case "LINT", "ULINT", "LREAL", "LWORD":
		return 8
	case "STRING":
		return 256
	}
	return 2
}

func parseS7Address(addr string) (*s7Tag, error) {
	full := strings.TrimSpace(addr)
	i := strings.LastIndex(full, ":")
	if i <= 0 {
		return nil, fmt.Errorf("invalid s7 address %q (expected <address>:<type>)", addr)
	}
	loc := strings.ToUpper(strings.TrimSpace(full[:i]))
	dt := strings.ToUpper(strings.TrimSpace(full[i+1:]))
	t := &s7Tag{raw: addr, bit: -1, dataType: dt}
	if m := s7DBRe.FindStringSubmatch(loc); m != nil {
		t.area = 'D'
		t.db, _ = strconv.Atoi(m[1])
		t.offset, _ = strconv.Atoi(m[3])
		if m[4] != "" {
			t.bit, _ = strconv.Atoi(m[4])
		}
	} else if m := s7AreaRe.FindStringSubmatch(loc); m != nil {
		switch m[1] {
		case "M":
			t.area = 'M'
		case "I", "E":
			t.area = 'I'
		case "Q", "A":
			t.area = 'Q'
		}
		t.offset, _ = strconv.Atoi(m[3])
		if m[4] != "" {
			t.bit, _ = strconv.Atoi(m[4])
		}
	} else {
		return nil, fmt.Errorf("invalid s7 address %q", addr)
	}
	if t.dataType == "BOOL" && t.bit < 0 {
		t.bit = 0
	}
	if t.bit > 7 {
		return nil, fmt.Errorf("invalid bit in s7 address %q", addr)
	}
	return t, nil
}

func decodeS7(t *s7Tag, b []byte) (string, error) {
	switch t.dataType {
	case "BOOL":
		return strconv.FormatBool(b[0]&(1<<uint(t.bit)) != 0), nil
	case "CHAR":
		return string(rune(b[0])), nil
	case "STRING":
		// S7 string: [max length][actual length][chars...]
		if len(b) < 2 {
			return "", errors.New("short string")
		}
		n := int(b[1])
		if n > len(b)-2 {
			n = len(b) - 2
		}
		return strings.ToValidUTF8(string(b[2:2+n]), ""), nil
	}
	return decodeBigEndian(b, t.dataType)
}

type s7Driver struct {
	mu      sync.Mutex
	handler *gos7.TCPClientHandler
	client  gos7.Client
}

func (d *s7Driver) open(g *Gateway) error {
	host := g.Param("IP_ADDRESS", "")
	if host == "" {
		return errors.New("IP_ADDRESS is required")
	}
	addr := net.JoinHostPort(host, strconv.Itoa(g.ParamInt("PORT", 102)))
	h := gos7.NewTCPClientHandler(addr, g.ParamInt("REMOTE_RACK", 0), g.ParamInt("REMOTE_SLOT", 1))
	h.Timeout = 10 * time.Second
	h.IdleTimeout = 60 * time.Second
	if err := h.Connect(); err != nil {
		return err
	}
	d.handler = h
	d.client = gos7.NewClient(h)
	return nil
}

func (d *s7Driver) close() {
	if d.handler != nil {
		_ = d.handler.Close()
	}
	d.handler = nil
	d.client = nil
}

type s7Block struct {
	area  byte
	db    int
	start int
	size  int
	tags  []*s7Tag
}

func (d *s7Driver) readAll(addresses []string) (map[string]string, error) {
	out := map[string]string{}
	var firstErr error
	var tags []*s7Tag
	for _, a := range addresses {
		t, err := parseS7Address(a)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		tags = append(tags, t)
	}
	sort.Slice(tags, func(i, j int) bool {
		if tags[i].area != tags[j].area {
			return tags[i].area < tags[j].area
		}
		if tags[i].db != tags[j].db {
			return tags[i].db < tags[j].db
		}
		return tags[i].offset < tags[j].offset
	})
	const maxBlock = 200
	var blocks []*s7Block
	for _, t := range tags {
		sz := t.size()
		if len(blocks) > 0 {
			b := blocks[len(blocks)-1]
			end := b.start + b.size
			if b.area == t.area && b.db == t.db && t.offset <= end+16 && t.offset+sz-b.start <= maxBlock {
				if t.offset+sz > end {
					b.size = t.offset + sz - b.start
				}
				b.tags = append(b.tags, t)
				continue
			}
		}
		blocks = append(blocks, &s7Block{area: t.area, db: t.db, start: t.offset, size: sz, tags: []*s7Tag{t}})
	}
	for _, b := range blocks {
		buf := make([]byte, b.size)
		var err error
		switch b.area {
		case 'D':
			err = d.client.AGReadDB(b.db, b.start, b.size, buf)
		case 'M':
			err = d.client.AGReadMB(b.start, b.size, buf)
		case 'I':
			err = d.client.AGReadEB(b.start, b.size, buf)
		case 'Q':
			err = d.client.AGReadAB(b.start, b.size, buf)
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, t := range b.tags {
			off := t.offset - b.start
			sz := t.size()
			if off+sz > len(buf) {
				sz = len(buf) - off
			}
			if sz <= 0 {
				continue
			}
			v, err := decodeS7(t, buf[off:off+sz])
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			out[t.raw] = v
		}
	}
	return out, firstErr
}

func (d *s7Driver) run(ctx context.Context, rt *runtime) {
	g := rt.gw
	defer func() {
		d.mu.Lock()
		d.close()
		d.mu.Unlock()
	}()
	pollLoop(ctx, g.Seconds("INTERVAL", time.Second), func(ctx context.Context) error {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.client == nil {
			if err := d.open(g); err != nil {
				return err
			}
		}
		addresses := make([]string, 0, len(g.Tags))
		for _, t := range g.Tags {
			addresses = append(addresses, t.Address)
		}
		res, errs := d.readAll(addresses)
		now := time.Now().UnixMilli()
		values := map[string]VTQC{}
		failed := 0
		for _, t := range g.Tags {
			if v, ok := res[t.Address]; ok {
				values[t.Name] = VTQC{V: v, T: now, Q: QualityGood}
			} else {
				failed++
				values[t.Name] = VTQC{V: "", T: now, Q: QualityBad}
			}
		}
		rt.update(values)
		if len(g.Tags) > 0 && failed == len(g.Tags) && errs != nil {
			d.close()
			return errs
		}
		rt.setConnected(true, errs)
		return nil
	}, func(err error) {
		rt.setConnected(false, err)
		rt.markAllBad()
	})
}

func (d *s7Driver) read(ctx context.Context, rt *runtime, addresses []string) (map[string]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	temp := false
	if d.client == nil {
		if err := d.open(rt.gw); err != nil {
			return nil, err
		}
		temp = true
	}
	res, err := d.readAll(addresses)
	if temp {
		d.close()
	}
	if len(res) == 0 && err != nil {
		return nil, err
	}
	return res, nil
}

func (d *s7Driver) browse(context.Context, *runtime, string, int) ([]*TagNode, error) {
	return nil, errUnsupported
}
