package agent

import (
	"encoding/binary"
	"encoding/hex"
	"testing"
)

// rawSum is the RFC1071 folded sum without the final complement; a packet
// carrying a correct checksum sums to 0xffff.
func rawSum(b []byte) uint32 {
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
	return sum
}

func TestICMPChecksum(t *testing.T) {
	// known vector: echo request skeleton, checksum field zeroed
	got := icmpChecksum([]byte{0x08, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01})
	if want := uint16(0xf7fd); got != want {
		t.Fatalf("checksum = %#04x, want %#04x", got, want)
	}

	// odd length: last byte counts as a high-order byte
	if got := icmpChecksum([]byte{0x08, 0x00, 0x00, 0x00, 0x01}); got != icmpChecksum([]byte{0x08, 0x00, 0x00, 0x00, 0x01, 0x00}) {
		t.Fatalf("odd-length padding not handled")
	}
}

func TestBuildICMPEchoRequest(t *testing.T) {
	pkt := buildICMPEchoRequest(0x1234, 0x0007, 0x0123456789abcdef)

	if len(pkt) != icmpHeaderLen+icmpPayloadLen {
		t.Fatalf("len = %d, want %d", len(pkt), icmpHeaderLen+icmpPayloadLen)
	}
	if pkt[0] != icmpTypeEchoRequest || pkt[1] != icmpCodeZero {
		t.Fatalf("type/code = %d/%d, want %d/%d", pkt[0], pkt[1], icmpTypeEchoRequest, icmpCodeZero)
	}
	if id := binary.BigEndian.Uint16(pkt[4:6]); id != 0x1234 {
		t.Fatalf("id = %#x, want 0x1234", id)
	}
	if seq := binary.BigEndian.Uint16(pkt[6:8]); seq != 0x0007 {
		t.Fatalf("seq = %#x, want 0x7", seq)
	}
	if ts := binary.BigEndian.Uint64(pkt[8:]); ts != 0x0123456789abcdef {
		t.Fatalf("payload ts = %#x, want 0x0123456789abcdef", ts)
	}
	if rawSum(pkt) != 0xffff {
		t.Fatalf("checksum invalid: folded sum = %#x, want 0xffff", rawSum(pkt))
	}

	// deterministic: same inputs, same bytes
	again := buildICMPEchoRequest(0x1234, 0x0007, 0x0123456789abcdef)
	if hex.EncodeToString(pkt) != hex.EncodeToString(again) {
		t.Fatalf("request build not deterministic")
	}
}

func TestFindEchoReply(t *testing.T) {
	body := buildICMPEchoRequest(0xabcd, 0x42, 12345)
	// a reply echoes everything but flips the type byte
	reply := append([]byte(nil), body...)
	reply[0] = icmpTypeEchoReply

	// with IPv4 header prepended (as linux SOCK_RAW delivers)
	hdr := []byte{0x45, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 64, 1, 0, 0, 10, 0, 0, 1, 10, 0, 0, 2}
	hdr[2] = byte((20 + len(reply)) >> 8)
	hdr[3] = byte(20 + len(reply))
	withIP := append(append([]byte{}, hdr...), reply...)

	rep, ok := findEchoReply(withIP)
	if !ok {
		t.Fatalf("echo reply not found behind IP header")
	}
	if rep.ID != 0xabcd || rep.Seq != 0x42 {
		t.Fatalf("id/seq = %#x/%#x, want 0xabcd/0x42", rep.ID, rep.Seq)
	}
	if got := binary.BigEndian.Uint64(rep.Payload); got != 12345 {
		t.Fatalf("payload = %d, want 12345", got)
	}

	// without IP header (e.g. after manual strip)
	rep, ok = findEchoReply(reply)
	if !ok || rep.ID != 0xabcd || rep.Seq != 0x42 {
		t.Fatalf("bare reply mismatch: ok=%v rep=%+v", ok, rep)
	}

	// request type must not match
	if _, ok := findEchoReply(body); ok {
		t.Fatalf("echo request matched as reply")
	}
	// truncated packet must not match
	if _, ok := findEchoReply(reply[:len(reply)-1]); ok {
		t.Fatalf("truncated packet matched")
	}
	// garbage IPv4 version byte short packet: just ignored
	if _, ok := findEchoReply([]byte{0x45, 0x00, 0x01}); ok {
		t.Fatalf("garbage matched")
	}
}
