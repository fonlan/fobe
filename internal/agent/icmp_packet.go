package agent

import "encoding/binary"

// ICMP echo wire format (design §13): raw IPv4 ICMP, id = pid & 0xffff,
// sequence auto-increment, payload carries the send timestamp so replies can
// be matched and validated. Pure helpers live here so every platform can
// test them; the raw socket itself is icmp_linux.go.
const (
	icmpTypeEchoRequest = 8
	icmpTypeEchoReply   = 0
	icmpCodeZero        = 0
	icmpHeaderLen       = 8 // type(1) code(1) checksum(2) id(2) seq(2)
	icmpPayloadLen      = 8 // unix nanoseconds, big endian
)

// icmpChecksum is the RFC1071 internet checksum over the ICMP segment.
func icmpChecksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum >> 16) + (sum & 0xffff)
	}
	return ^uint16(sum)
}

// buildICMPEchoRequest assembles an echo request with the given id/seq and
// sentNano (unix ns) as payload, checksum included.
func buildICMPEchoRequest(id, seq uint16, sentNano int64) []byte {
	pkt := make([]byte, icmpHeaderLen+icmpPayloadLen)
	pkt[0] = icmpTypeEchoRequest
	pkt[1] = icmpCodeZero
	binary.BigEndian.PutUint16(pkt[4:6], id)
	binary.BigEndian.PutUint16(pkt[6:8], seq)
	binary.BigEndian.PutUint64(pkt[icmpHeaderLen:], uint64(sentNano))
	binary.BigEndian.PutUint16(pkt[2:4], icmpChecksum(pkt))
	return pkt
}

// echoReply holds the matched fields of an echo reply.
type echoReply struct {
	ID      uint16
	Seq     uint16
	Payload []byte
}

// findEchoReply locates an echo reply in a received datagram, skipping the
// IPv4 header when the raw socket prepended one. Non-reply or malformed
// packets return ok=false so the caller keeps reading.
func findEchoReply(pkt []byte) (echoReply, bool) {
	// Linux SOCK_RAW/IPPROTO_ICMP delivers the IP header; strip it (RFC 791 IHL).
	if len(pkt) >= 20 && pkt[0]>>4 == 4 {
		ihl := int(pkt[0]&0x0f) * 4
		if ihl < 20 || len(pkt) < ihl+icmpHeaderLen {
			return echoReply{}, false
		}
		pkt = pkt[ihl:]
	}
	if len(pkt) < icmpHeaderLen+icmpPayloadLen {
		return echoReply{}, false
	}
	if pkt[0] != icmpTypeEchoReply {
		return echoReply{}, false
	}
	return echoReply{
		ID:      binary.BigEndian.Uint16(pkt[4:6]),
		Seq:     binary.BigEndian.Uint16(pkt[6:8]),
		Payload: pkt[icmpHeaderLen : icmpHeaderLen+icmpPayloadLen],
	}, true
}
