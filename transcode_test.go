package main

import (
	"testing"

	"github.com/pion/rtp"
)

// capturingTrack records what the forwarder writes, standing in for the
// PCMA-typed pion track the hub would hand over.
type capturingTrack struct {
	written []*rtp.Packet
}

func (c *capturingTrack) WriteRTP(p *rtp.Packet) error {
	clone := *p
	clone.Payload = append([]byte(nil), p.Payload...)
	c.written = append(c.written, &clone)
	return nil
}

func alawPacket(payload []byte) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 8, SequenceNumber: 100, Timestamp: 16000},
		Payload: payload,
	}
}

// TestPCMAPassesThroughUntouched is the regression test for the Tiandy
// cameras: they negotiate PCMA, which is already the codec our WebRTC track
// advertises. Running it through the L16 transcoder reinterprets A-law
// bytes as big-endian linear samples and halves an already-8 kHz timestamp,
// so the browser plays noise at half speed.
func TestPCMAPassesThroughUntouched(t *testing.T) {
	track := &capturingTrack{}
	forward := audioForwarder("PCMA", track)
	if forward == nil {
		t.Fatal("audioForwarder(PCMA) = nil, want a forwarder")
	}

	in := alawPacket([]byte{0xD5, 0x55, 0xD4, 0x2A, 0xAB, 0x7F})
	if err := forward(in); err != nil {
		t.Fatalf("forward: %v", err)
	}

	if len(track.written) != 1 {
		t.Fatalf("wrote %d packets, want 1", len(track.written))
	}
	got := track.written[0]
	if string(got.Payload) != string(in.Payload) {
		t.Errorf("payload = % x, want % x (unmodified)", got.Payload, in.Payload)
	}
	if got.Timestamp != in.Timestamp {
		t.Errorf("timestamp = %d, want %d (PCMA is already an 8 kHz clock)", got.Timestamp, in.Timestamp)
	}
}

// TestL16IsTranscodedToPCMA keeps the Dahua VTO path working: L16/16000 is
// not a codec browsers accept, so it must still be decimated and A-law
// encoded, with the timestamp halved to match the 8 kHz output clock.
func TestL16IsTranscodedToPCMA(t *testing.T) {
	track := &capturingTrack{}
	forward := audioForwarder("L16", track)
	if forward == nil {
		t.Fatal("audioForwarder(L16) = nil, want a forwarder")
	}

	// Four big-endian samples → two A-law bytes after 2:1 decimation.
	in := &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: 7, Timestamp: 32000},
		Payload: []byte{0x10, 0x00, 0x10, 0x00, 0xF0, 0x00, 0xF0, 0x00},
	}
	if err := forward(in); err != nil {
		t.Fatalf("forward: %v", err)
	}

	got := track.written[0]
	want := []byte{linearToALaw(0x1000), linearToALaw(int16(0xF000 - 0x10000))}
	if string(got.Payload) != string(want) {
		t.Errorf("payload = % x, want % x", got.Payload, want)
	}
	if got.Timestamp != 16000 {
		t.Errorf("timestamp = %d, want 16000 (halved from 32000)", got.Timestamp)
	}
}

// TestPCMUIsConvertedToALaw covers the third codec findMedia accepts. The
// track advertises PCMA, so u-law bytes have to be converted rather than
// passed through as if they were A-law.
func TestPCMUIsConvertedToALaw(t *testing.T) {
	track := &capturingTrack{}
	forward := audioForwarder("PCMU", track)
	if forward == nil {
		t.Fatal("audioForwarder(PCMU) = nil, want a forwarder")
	}

	in := &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: 3, Timestamp: 8000},
		Payload: []byte{0xFF, 0x7F, 0x00},
	}
	if err := forward(in); err != nil {
		t.Fatalf("forward: %v", err)
	}

	got := track.written[0]
	want := []byte{
		linearToALaw(uLawToLinear(0xFF)),
		linearToALaw(uLawToLinear(0x7F)),
		linearToALaw(uLawToLinear(0x00)),
	}
	if string(got.Payload) != string(want) {
		t.Errorf("payload = % x, want % x", got.Payload, want)
	}
	if got.Timestamp != in.Timestamp {
		t.Errorf("timestamp = %d, want %d (both clocks are 8 kHz)", got.Timestamp, in.Timestamp)
	}
}

// TestUnknownCodecHasNoForwarder means the stream stays video-only rather
// than shipping garbage to the browser.
func TestUnknownCodecHasNoForwarder(t *testing.T) {
	if f := audioForwarder("MPEG4-GENERIC", &capturingTrack{}); f != nil {
		t.Error("audioForwarder(MPEG4-GENERIC) returned a forwarder, want nil")
	}
}

// TestAudioCodecsAreAllForwardable keeps findMedia's accept list and the
// forwarder's switch from drifting apart — that drift is what let PCMA
// reach the L16 transcoder in the first place.
func TestAudioCodecsAreAllForwardable(t *testing.T) {
	for _, codec := range audioCodecs {
		if f := audioForwarder(codec, &capturingTrack{}); f == nil {
			t.Errorf("findMedia accepts %s but audioForwarder has no case for it", codec)
		}
	}
}
