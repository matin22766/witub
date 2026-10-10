package client

import "encoding/binary"

const dnsTypeAAAA = 28

// emptyAAAAResponse returns a NOERROR response with no answers if msg is a
// single-question AAAA query.
func emptyAAAAResponse(msg []byte) ([]byte, bool) {
	if len(msg) < 12 || msg[2]&0x80 != 0 || binary.BigEndian.Uint16(msg[4:]) != 1 {
		return nil, false
	}
	i := 12
	for {
		if i >= len(msg) {
			return nil, false
		}
		l := int(msg[i])
		if l == 0 {
			i++
			break
		}
		if l&0xc0 != 0 { // compression pointer; not expected in a question
			return nil, false
		}
		i += 1 + l
	}
	if i+4 > len(msg) || binary.BigEndian.Uint16(msg[i:]) != dnsTypeAAAA {
		return nil, false
	}
	end := i + 4
	resp := append([]byte(nil), msg[:end]...)
	resp[2] = (resp[2] | 0x80) &^ 0x04       // QR=1, AA=0; keep opcode and RD
	resp[3] = 0x80                           // RA=1, RCODE=NOERROR
	binary.BigEndian.PutUint16(resp[6:], 0)  // ANCOUNT
	binary.BigEndian.PutUint16(resp[8:], 0)  // NSCOUNT
	binary.BigEndian.PutUint16(resp[10:], 0) // ARCOUNT
	return resp, true
}
