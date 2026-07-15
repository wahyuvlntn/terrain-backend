package datahub

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/simonvetter/modbus"
)

// Modbus tag address: <type digit><offset>[.<bit|length>]:<DATATYPE>{unit-id: 1, byte-order: "BIG_ENDIAN"}
// type digit: 0 coil, 1 discrete input, 3 input register, 4 holding register.
var modbusAddrRe = regexp.MustCompile(`^\s*([0134])(\d+)(?:\.(\d+))?\s*(?::\s*([A-Za-z_]+))?\s*(?:\{(.*)\})?\s*$`)
var modbusKVRe = regexp.MustCompile(`([\w\-]+)\s*:\s*(-?\d+\.\d+|-?\d+|"[^"]*"|'[^']*'|true|false)`)

type modbusTag struct {
	raw       string
	area      byte // '0','1','3','4'
	offset    uint16
	bit       int // -1 when unused; for STRING: register count
	dataType  string
	unitID    uint8
	byteOrder string
}

func parseModbusAddress(addr string, zeroBased bool, defUnit uint8, defOrder string) (*modbusTag, error) {
	m := modbusAddrRe.FindStringSubmatch(addr)
	if m == nil {
		return nil, fmt.Errorf("invalid modbus address %q", addr)
	}
	off, err := strconv.Atoi(m[2])
	if err != nil {
		return nil, fmt.Errorf("invalid modbus address %q", addr)
	}
	if !zeroBased {
		off--
	}
	if off < 0 || off > 65535 {
		return nil, fmt.Errorf("modbus address %q out of range", addr)
	}
	t := &modbusTag{raw: addr, area: m[1][0], offset: uint16(off), bit: -1, dataType: strings.ToUpper(m[4]), unitID: defUnit, byteOrder: defOrder}
	if t.dataType == "" {
		if t.area == '0' || t.area == '1' {
			t.dataType = "BOOL"
		} else {
			t.dataType = "INT"
		}
	}
	if m[3] != "" {
		n, _ := strconv.Atoi(m[3])
		t.bit = n
	}
	for _, kv := range modbusKVRe.FindAllStringSubmatch(m[5], -1) {
		val := strings.Trim(kv[2], `"'`)
		switch kv[1] {
		case "unit-id":
			if n, err := strconv.Atoi(val); err == nil && n >= 0 && n <= 255 {
				t.unitID = uint8(n)
			}
		case "byte-order":
			if val != "" {
				t.byteOrder = strings.ToUpper(val)
			}
		}
	}
	return t, nil
}

// registers returns the number of 16 bit registers needed.
func (t *modbusTag) registers() int {
	if t.area == '0' || t.area == '1' {
		return 1
	}
	switch t.dataType {
	case "BOOL", "INT", "UINT", "SINT", "USINT", "WORD":
		return 1
	case "DINT", "UDINT", "REAL", "DWORD":
		return 2
	case "LINT", "ULINT", "LREAL", "LWORD":
		return 4
	case "STRING":
		if t.bit > 0 {
			return t.bit
		}
		return 10
	}
	return 1
}

// reorder converts register bytes (wire order) into big endian byte order.
func reorder(b []byte, order string) []byte {
	out := append([]byte(nil), b...)
	switch order {
	case "LITTLE_ENDIAN":
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	case "BIG_ENDIAN_BYTE_SWAP":
		for i := 0; i+1 < len(out); i += 2 {
			out[i], out[i+1] = out[i+1], out[i]
		}
	case "LITTLE_ENDIAN_BYTE_SWAP":
		// reverse word order, keep bytes inside words
		words := len(out) / 2
		for i := 0; i < words/2; i++ {
			a, z := i*2, (words-1-i)*2
			out[a], out[z] = out[z], out[a]
			out[a+1], out[z+1] = out[z+1], out[a+1]
		}
	}
	return out
}

// decodeBigEndian interprets b (big endian) as the given PLC data type.
func decodeBigEndian(b []byte, dataType string) (string, error) {
	need := map[string]int{"SINT": 1, "USINT": 1, "BYTE": 1, "INT": 2, "UINT": 2, "WORD": 2, "DINT": 4, "UDINT": 4, "DWORD": 4, "REAL": 4,
		"LINT": 8, "ULINT": 8, "LWORD": 8, "LREAL": 8}
	if n, ok := need[dataType]; ok && len(b) < n {
		return "", fmt.Errorf("not enough data for %s", dataType)
	}
	switch dataType {
	case "SINT":
		return strconv.Itoa(int(int8(b[0]))), nil
	case "USINT", "BYTE":
		return strconv.Itoa(int(b[0])), nil
	case "INT":
		return strconv.Itoa(int(int16(binary.BigEndian.Uint16(b)))), nil
	case "UINT", "WORD":
		return strconv.Itoa(int(binary.BigEndian.Uint16(b))), nil
	case "DINT":
		return strconv.FormatInt(int64(int32(binary.BigEndian.Uint32(b))), 10), nil
	case "UDINT", "DWORD":
		return strconv.FormatUint(uint64(binary.BigEndian.Uint32(b)), 10), nil
	case "REAL":
		return formatFloat(float64(math.Float32frombits(binary.BigEndian.Uint32(b)))), nil
	case "LINT":
		return strconv.FormatInt(int64(binary.BigEndian.Uint64(b)), 10), nil
	case "ULINT", "LWORD":
		return strconv.FormatUint(binary.BigEndian.Uint64(b), 10), nil
	case "LREAL":
		return formatFloat(math.Float64frombits(binary.BigEndian.Uint64(b))), nil
	case "STRING":
		s := strings.TrimRight(string(b), "\x00 ")
		return strings.ToValidUTF8(s, ""), nil
	}
	return "", fmt.Errorf("unsupported data type %s", dataType)
}

func formatFloat(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "NaN"
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

type modbusDriver struct {
	mu     sync.Mutex
	client *modbus.ModbusClient
}

func (d *modbusDriver) open(g *Gateway) (*modbus.ModbusClient, error) {
	cfg := &modbus.ClientConfiguration{Logger: log.New(io.Discard, "", 0)}
	if g.Type == ModbusRTU {
		port := g.Param("SERIAL_PORT", "")
		if port == "" {
			return nil, errors.New("SERIAL_PORT is required")
		}
		cfg.URL = "rtu://" + port
		cfg.Speed = uint(g.ParamInt("BAUD_RATE", 9600))
		cfg.DataBits = uint(g.ParamInt("DATA_BITS", 8))
		cfg.StopBits = uint(g.ParamInt("STOP_BITS", 1))
		switch strings.ToUpper(g.Param("PARITY", "NONE")) {
		case "ODD":
			cfg.Parity = modbus.PARITY_ODD
		case "EVEN":
			cfg.Parity = modbus.PARITY_EVEN
		default:
			cfg.Parity = modbus.PARITY_NONE
		}
	} else {
		host := g.Param("IP_ADDRESS", "")
		if host == "" {
			return nil, errors.New("IP_ADDRESS is required")
		}
		cfg.URL = fmt.Sprintf("tcp://%s:%d", host, g.ParamInt("PORT", 502))
	}
	cfg.Timeout = g.Seconds("TIMEOUT_S", 5*time.Second)
	c, err := modbus.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	if err := c.Open(); err != nil {
		return nil, err
	}
	return c, nil
}

func (d *modbusDriver) interval(g *Gateway) time.Duration {
	if g.Type == ModbusRTU {
		return g.Seconds("INTERVAL_S", time.Second)
	}
	return g.Seconds("INTERVAL", time.Second)
}

func (d *modbusDriver) defaults(g *Gateway) (bool, uint8, string) {
	unit := g.ParamInt("DEFAULT_UNIT_IDENTIFIER", 1)
	if unit < 0 || unit > 255 {
		unit = 1
	}
	return g.ParamBool("ZERO_BASED_ADDRESSING", false), uint8(unit), strings.ToUpper(g.Param("DEFAULT_PAYLOAD_BYTE_ORDER", "BIG_ENDIAN"))
}

func (d *modbusDriver) run(ctx context.Context, rt *runtime) {
	g := rt.gw
	defer d.closeClient()
	pollLoop(ctx, d.interval(g), func(ctx context.Context) error {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.client == nil {
			c, err := d.open(g)
			if err != nil {
				return err
			}
			d.client = c
		}
		addresses := make([]string, 0, len(g.Tags))
		for _, t := range g.Tags {
			addresses = append(addresses, t.Address)
		}
		res, errs := d.readAll(d.client, g, addresses)
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
			// Assume the connection is broken; reopen next time.
			_ = d.client.Close()
			d.client = nil
			return errs
		}
		rt.setConnected(true, errs)
		return nil
	}, func(err error) {
		rt.setConnected(false, err)
		rt.markAllBad()
	})
}

func (d *modbusDriver) closeClient() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.client != nil {
		_ = d.client.Close()
		d.client = nil
	}
}

type modbusBlock struct {
	unit  uint8
	area  byte
	start uint16
	count int
	tags  []*modbusTag
}

// readAll reads every address, merging adjacent registers into blocks.
func (d *modbusDriver) readAll(c *modbus.ModbusClient, g *Gateway, addresses []string) (map[string]string, error) {
	zero, unit, order := d.defaults(g)
	out := map[string]string{}
	var firstErr error
	var tags []*modbusTag
	for _, a := range addresses {
		t, err := parseModbusAddress(a, zero, unit, order)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		tags = append(tags, t)
	}
	sort.Slice(tags, func(i, j int) bool {
		if tags[i].unitID != tags[j].unitID {
			return tags[i].unitID < tags[j].unitID
		}
		if tags[i].area != tags[j].area {
			return tags[i].area < tags[j].area
		}
		return tags[i].offset < tags[j].offset
	})
	var blocks []*modbusBlock
	for _, t := range tags {
		n := t.registers()
		maxSpan := 120
		if t.area == '0' || t.area == '1' {
			maxSpan = 1000
		}
		if len(blocks) > 0 {
			b := blocks[len(blocks)-1]
			end := int(b.start) + b.count
			if b.unit == t.unitID && b.area == t.area && int(t.offset) <= end+8 && int(t.offset)+n-int(b.start) <= maxSpan {
				if int(t.offset)+n > end {
					b.count = int(t.offset) + n - int(b.start)
				}
				b.tags = append(b.tags, t)
				continue
			}
		}
		blocks = append(blocks, &modbusBlock{unit: t.unitID, area: t.area, start: t.offset, count: n, tags: []*modbusTag{t}})
	}
	for _, b := range blocks {
		if err := c.SetUnitId(b.unit); err != nil {
			firstErr = err
			continue
		}
		switch b.area {
		case '0', '1':
			var bits []bool
			var err error
			if b.area == '0' {
				bits, err = c.ReadCoils(b.start, uint16(b.count))
			} else {
				bits, err = c.ReadDiscreteInputs(b.start, uint16(b.count))
			}
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			for _, t := range b.tags {
				i := int(t.offset - b.start)
				if i < len(bits) {
					out[t.raw] = strconv.FormatBool(bits[i])
				}
			}
		default:
			rt := modbus.HOLDING_REGISTER
			if b.area == '3' {
				rt = modbus.INPUT_REGISTER
			}
			regs, err := c.ReadRegisters(b.start, uint16(b.count), rt)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			for _, t := range b.tags {
				i := int(t.offset - b.start)
				n := t.registers()
				if i+n > len(regs) {
					continue
				}
				raw := make([]byte, n*2)
				for k := 0; k < n; k++ {
					binary.BigEndian.PutUint16(raw[k*2:], regs[i+k])
				}
				if t.dataType == "BOOL" {
					word := binary.BigEndian.Uint16(reorder(raw[:2], t.byteOrder))
					bit := t.bit
					if bit < 0 {
						out[t.raw] = strconv.FormatBool(word != 0)
					} else if bit < 16 {
						out[t.raw] = strconv.FormatBool(word&(1<<uint(bit)) != 0)
					}
					continue
				}
				var v string
				if t.dataType == "STRING" {
					v, err = decodeBigEndian(raw, "STRING")
				} else {
					v, err = decodeBigEndian(reorder(raw, t.byteOrder), t.dataType)
				}
				if err == nil {
					out[t.raw] = v
				} else if firstErr == nil {
					firstErr = err
				}
			}
		}
	}
	return out, firstErr
}

func (d *modbusDriver) read(ctx context.Context, rt *runtime, addresses []string) (map[string]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.client
	temp := false
	if c == nil {
		var err error
		c, err = d.open(rt.gw)
		if err != nil {
			return nil, err
		}
		temp = true
	}
	res, err := d.readAll(c, rt.gw, addresses)
	if temp {
		_ = c.Close()
	}
	if len(res) == 0 && err != nil {
		return nil, err
	}
	return res, nil
}

func (d *modbusDriver) browse(context.Context, *runtime, string, int) ([]*TagNode, error) {
	return nil, errUnsupported
}
