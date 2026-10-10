package proto

import (
	"bytes"
	"errors"
	"net/netip"
	"testing"
)

func TestOpenRoundTrip(t *testing.T) {
	cases := []OpenRequest{
		{Kind: KindTCP, Addr: Addr{IP: netip.MustParseAddr("93.184.216.34"), Port: 443}},
		{Kind: KindUDP, Addr: Addr{IP: netip.MustParseAddr("2606:4700::1111"), Port: 53}},
		{Kind: KindTCP, Addr: Addr{Domain: "example.com", Port: 80}},
		{Kind: KindDNS},
	}
	for _, want := range cases {
		var buf bytes.Buffer
		if err := WriteOpen(&buf, want); err != nil {
			t.Fatal(err)
		}
		got, err := ReadOpen(&buf)
		if err != nil || got != want {
			t.Fatalf("got %+v, %v; want %+v", got, err, want)
		}
		if buf.Len() != 0 {
			t.Fatalf("%d trailing bytes", buf.Len())
		}
	}
}

func TestOpenRejectsGarbage(t *testing.T) {
	if _, err := ReadOpen(bytes.NewReader([]byte{9})); err == nil {
		t.Fatal("unknown kind accepted")
	}
	if _, err := ReadOpen(bytes.NewReader([]byte{byte(KindTCP), 7})); err == nil {
		t.Fatal("unknown address type accepted")
	}
	if err := WriteOpen(&bytes.Buffer{}, OpenRequest{Kind: KindTCP}); err == nil {
		t.Fatal("empty address accepted")
	}
}

func TestHandshake(t *testing.T) {
	var buf bytes.Buffer
	WriteHandshake(&buf, "tok")
	if tok, err := ReadHandshake(&buf); err != nil || tok != "tok" {
		t.Fatalf("got %q, %v", tok, err)
	}
	if _, err := ReadHandshake(bytes.NewReader([]byte("HTTP/"))); err == nil {
		t.Fatal("bad magic accepted")
	}
}

func TestStatus(t *testing.T) {
	var buf bytes.Buffer
	WriteStatus(&buf, StatusTimeout)
	if err := ReadStatus(&buf); !errors.Is(err, StatusTimeout) {
		t.Fatalf("got %v", err)
	}
}

func TestDatagram(t *testing.T) {
	var buf bytes.Buffer
	WriteDatagram(&buf, []byte("abc"))
	WriteDatagram(&buf, nil)
	out := make([]byte, MaxDatagram)
	if n, err := ReadDatagram(&buf, out); err != nil || string(out[:n]) != "abc" {
		t.Fatalf("got %q, %v", out[:n], err)
	}
	if n, err := ReadDatagram(&buf, out); err != nil || n != 0 {
		t.Fatalf("empty datagram: %d, %v", n, err)
	}
}
