// Package calltrace turns the SIP messages an edge's components capture (as HEP) and LiveKit's webhooks into one
// OpenTelemetry trace per call: a span for each SIP transaction on each hop, nested hop by hop, and LiveKit's room
// and participants under the hop that reached it. cmd/call-tracer runs it.
package calltrace

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"time"
)

// HEP payload types (chunk 11). Only SIP is traced.
const hepProtoSIP = 1

// Packet is one message captured and sent as HEPv3.
type Packet struct {
	// Time the capturing component saw the message.
	Time     time.Time
	Src, Dst netip.AddrPort
	// Transport is the IP protocol number: 17 UDP, 6 TCP.
	Transport uint8
	// PayloadType is HEP's protocol type: 1 for SIP.
	PayloadType uint8
	// CaptureID names the component that captured the message.
	CaptureID uint32
	Payload   []byte
}

// ErrNotHEP3 is returned for a datagram that isn't HEPv3 (HEPv1 and v2 aren't supported).
var ErrNotHEP3 = errors.New("not a HEPv3 packet")

// DecodeHEP decodes one HEPv3 packet (https://github.com/sipcapture/HEP). Chunks of other vendors, and generic
// chunks it doesn't use, are skipped. Payload aliases b.
func DecodeHEP(b []byte) (Packet, error) {
	var p Packet
	if len(b) < 6 || string(b[:4]) != "HEP3" {
		return p, ErrNotHEP3
	}
	total := int(binary.BigEndian.Uint16(b[4:6]))
	if total < 6 || total > len(b) {
		return p, fmt.Errorf("HEP length %d, datagram %d bytes", total, len(b))
	}
	b = b[:total]
	var srcIP, dstIP netip.Addr
	var srcPort, dstPort uint16
	var sec, usec uint32
	for off := 6; off < total; {
		if total-off < 6 {
			return p, fmt.Errorf("truncated chunk header at %d", off)
		}
		vendor := binary.BigEndian.Uint16(b[off:])
		typ := binary.BigEndian.Uint16(b[off+2:])
		n := int(binary.BigEndian.Uint16(b[off+4:]))
		if n < 6 || off+n > total {
			return p, fmt.Errorf("chunk %d: bad length %d", typ, n)
		}
		v := b[off+6 : off+n]
		off += n
		if vendor != 0 {
			continue
		}
		var ok bool
		switch typ {
		case 2:
			p.Transport, ok = u8(v)
		case 3, 5:
			srcIP, ok = netip.AddrFromSlice(v)
		case 4, 6:
			dstIP, ok = netip.AddrFromSlice(v)
		case 7:
			srcPort, ok = u16(v)
		case 8:
			dstPort, ok = u16(v)
		case 9:
			sec, ok = u32(v)
		case 10:
			usec, ok = u32(v)
		case 11:
			p.PayloadType, ok = u8(v)
		case 12:
			p.CaptureID, ok = u32(v)
		case 15:
			p.Payload, ok = v, true
		case 16:
			return p, errors.New("compressed HEP payloads aren't supported")
		default:
			ok = true
		}
		if !ok {
			return p, fmt.Errorf("chunk %d: bad value length %d", typ, len(v))
		}
	}
	if p.Payload == nil {
		return p, errors.New("HEP packet without a payload")
	}
	p.Src = netip.AddrPortFrom(srcIP.Unmap(), srcPort)
	p.Dst = netip.AddrPortFrom(dstIP.Unmap(), dstPort)
	p.Time = time.Unix(int64(sec), int64(usec)*1000)
	return p, nil
}

func u8(v []byte) (uint8, bool) {
	if len(v) != 1 {
		return 0, false
	}
	return v[0], true
}

func u16(v []byte) (uint16, bool) {
	if len(v) != 2 {
		return 0, false
	}
	return binary.BigEndian.Uint16(v), true
}

func u32(v []byte) (uint32, bool) {
	if len(v) != 4 {
		return 0, false
	}
	return binary.BigEndian.Uint32(v), true
}

// EncodeHEP encodes p as HEPv3, for tests and tools that replay captures.
func EncodeHEP(p Packet) []byte {
	b := []byte("HEP3\x00\x00")
	chunk := func(typ uint16, v []byte) {
		b = binary.BigEndian.AppendUint16(b, 0)
		b = binary.BigEndian.AppendUint16(b, typ)
		b = binary.BigEndian.AppendUint16(b, uint16(6+len(v)))
		b = append(b, v...)
	}
	src, dst := p.Src.Addr(), p.Dst.Addr()
	if src.Is4() {
		chunk(1, []byte{2})
		chunk(3, src.AsSlice())
		chunk(4, dst.AsSlice())
	} else {
		chunk(1, []byte{10})
		chunk(5, src.AsSlice())
		chunk(6, dst.AsSlice())
	}
	chunk(2, []byte{p.Transport})
	chunk(7, binary.BigEndian.AppendUint16(nil, p.Src.Port()))
	chunk(8, binary.BigEndian.AppendUint16(nil, p.Dst.Port()))
	chunk(9, binary.BigEndian.AppendUint32(nil, uint32(p.Time.Unix())))
	chunk(10, binary.BigEndian.AppendUint32(nil, uint32(p.Time.Nanosecond()/1000)))
	chunk(11, []byte{p.PayloadType})
	chunk(12, binary.BigEndian.AppendUint32(nil, p.CaptureID))
	chunk(15, p.Payload)
	binary.BigEndian.PutUint16(b[4:], uint16(len(b)))
	return b
}
