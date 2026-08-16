package main

import (
	"net"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
)

// TestForwarderProducesDecryptableStream is the closest thing to an iPhone
// this suite can offer: it stands up a UDP listener, pushes packets shaped
// like the camera's through the forwarder, and decrypts what comes out
// with the key HomeKit would have supplied. If the crypto or the header
// rewrite is wrong, this fails.
func TestForwarderProducesDecryptableStream(t *testing.T) {
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	key, salt := randomSRTPKeySalt()
	const (
		wantSSRC = uint32(0xDEADBEEF)
		wantPT   = uint8(99)
	)

	sender, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("bind sender: %v", err)
	}

	fwd, err := newSRTPForwarder("vto1", sender, listener.LocalAddr().(*net.UDPAddr), key, salt, wantSSRC, wantPT, 1378)
	if err != nil {
		t.Fatalf("newSRTPForwarder: %v", err)
	}
	defer fwd.Close()

	decrypt, err := srtp.CreateContext(key, salt, srtp.ProtectionProfileAes128CmHmacSha1_80)
	if err != nil {
		t.Fatalf("decrypt context: %v", err)
	}

	// Shaped like a camera: its own SSRC and payload type, and a sequence
	// space that starts somewhere arbitrary and even skips.
	sent := []*rtp.Packet{
		{Header: rtp.Header{Version: 2, SSRC: 0x11111111, PayloadType: 96, SequenceNumber: 40000, Timestamp: 900000}, Payload: []byte{0x67, 0x42, 0x00, 0x1e}},
		{Header: rtp.Header{Version: 2, SSRC: 0x11111111, PayloadType: 96, SequenceNumber: 40001, Timestamp: 903000}, Payload: []byte{0x68, 0xce, 0x3c, 0x80}},
		{Header: rtp.Header{Version: 2, SSRC: 0x11111111, PayloadType: 96, SequenceNumber: 40009, Timestamp: 906000}, Payload: []byte{0x65, 0x88, 0x84, 0x00}},
	}

	var firstSeq uint16
	for i, p := range sent {
		if err := fwd.WriteRTP(p); err != nil {
			t.Fatalf("WriteRTP %d: %v", i, err)
		}

		buf := make([]byte, 1500)
		_ = listener.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, _, err := listener.ReadFrom(buf)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}

		plain, err := decrypt.DecryptRTP(nil, buf[:n], nil)
		if err != nil {
			t.Fatalf("decrypt %d: %v", i, err)
		}

		var got rtp.Packet
		if err := got.Unmarshal(plain); err != nil {
			t.Fatalf("unmarshal %d: %v", i, err)
		}

		if got.SSRC != wantSSRC {
			t.Errorf("packet %d SSRC = %#x, want the negotiated %#x", i, got.SSRC, wantSSRC)
		}
		if got.PayloadType != wantPT {
			t.Errorf("packet %d payload type = %d, want the negotiated %d", i, got.PayloadType, wantPT)
		}
		if got.Timestamp != p.Timestamp {
			t.Errorf("packet %d timestamp = %d, want %d passed through", i, got.Timestamp, p.Timestamp)
		}
		if string(got.Payload) != string(p.Payload) {
			t.Errorf("packet %d payload altered", i)
		}

		// The camera's sequence numbering is replaced by our own, which
		// must be gap-free — SRTP's replay window depends on it.
		if i == 0 {
			firstSeq = got.SequenceNumber
		} else if want := firstSeq + uint16(i); got.SequenceNumber != want {
			t.Errorf("packet %d sequence = %d, want %d (contiguous)", i, got.SequenceNumber, want)
		}
	}
}

func TestForwarderWriteAfterCloseIsHarmless(t *testing.T) {
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	key, salt := randomSRTPKeySalt()
	sender, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("bind sender: %v", err)
	}

	fwd, err := newSRTPForwarder("vto1", sender, listener.LocalAddr().(*net.UDPAddr), key, salt, 1, 99, 1378)
	if err != nil {
		t.Fatalf("newSRTPForwarder: %v", err)
	}

	// The RTSP reader runs in its own goroutine and can be mid-packet when
	// HomeKit ends the stream, so a write after Close must not error or
	// panic on a closed socket.
	if err := fwd.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	p := &rtp.Packet{Header: rtp.Header{Version: 2}, Payload: []byte{1, 2, 3}}
	if err := fwd.WriteRTP(p); err != nil {
		t.Errorf("WriteRTP after Close = %v, want nil", err)
	}
	if err := fwd.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

func TestSubtypeForWidth(t *testing.T) {
	tests := []struct {
		width int
		want  int
	}{
		{320, 1},  // Apple Watch
		{640, 1},  // small tile
		{1279, 1}, // just below the main-stream threshold
		{1280, 0}, // 720p
		{1920, 0}, // 1080p
	}
	for _, tt := range tests {
		if got := subtypeForWidth(tt.width); got != tt.want {
			t.Errorf("subtypeForWidth(%d) = %d, want %d", tt.width, got, tt.want)
		}
	}
}
