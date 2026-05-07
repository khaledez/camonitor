// Audio transcoding from the camera's wire format to a codec the browser's
// WebRTC stack actually decodes.
//
// Dahua VTOs we've tested send L16/16000 (uncompressed 16-bit linear PCM,
// big-endian samples, 16 kHz, mono). Browsers don't accept L16 in WebRTC —
// PCMA (G.711 A-law @ 8 kHz) is the closest mandatory-to-implement codec
// for narrowband voice. So for each inbound L16 RTP packet we:
//
//  1. Read each 2-byte sample as a big-endian int16 (RFC 3551 §4.5.10).
//  2. Decimate 2:1 with a simple averaging low-pass to drop to 8 kHz —
//     adequate for voice, no need for a real FIR.
//  3. Map each linear sample to an A-law byte via the standard table.
//  4. Halve the RTP timestamp so the 8 kHz clock is consistent with the
//     PCMA payload pion will be sending.
//
// The resulting *rtp.Packet is written to the supplied trackWriter (a
// PCMA-typed pion track). Pion rewrites SSRC and payload type; sequence
// numbers are passed through.
package main

import (
	"encoding/binary"

	"github.com/pion/rtp"
)

// l16ToPCMAForwarder returns a function that transcodes inbound L16/16000
// mono packets to PCMA/8000 mono and writes them to track.
func l16ToPCMAForwarder(track trackWriter) func(*rtp.Packet) error {
	out := &rtp.Packet{}
	return func(in *rtp.Packet) error {
		samples := len(in.Payload) / 2
		if samples < 2 {
			return nil
		}
		// Decimate by averaging adjacent samples — keeps things small and
		// avoids obvious aliasing on speech.
		alaw := make([]byte, samples/2)
		src := in.Payload
		for i := 0; i < samples-1; i += 2 {
			s0 := int32(int16(binary.BigEndian.Uint16(src[i*2:])))
			s1 := int32(int16(binary.BigEndian.Uint16(src[(i+1)*2:])))
			alaw[i/2] = linearToALaw(int16((s0 + s1) / 2))
		}
		out.Header = in.Header
		// L16 timestamps tick at 16 kHz, PCMA at 8 kHz — halve to match.
		out.Header.Timestamp = in.Header.Timestamp / 2
		out.Payload = alaw
		return track.WriteRTP(out)
	}
}

// linearToALaw converts a signed 16-bit linear PCM sample to an 8-bit
// G.711 A-law byte. Implementation follows the canonical reference in
// CCITT G.711 / ITU-T Rec. G.191 (sftools/alaw.c). Inverted-bit XOR
// (0x55) is part of the A-law spec to spread DC bias across line states.
func linearToALaw(pcm int16) byte {
	const cClip = 32635
	var sign byte
	if pcm >= 0 {
		sign = 0xD5 // 0x55 ^ 0x80
	} else {
		sign = 0x55
		pcm = ^pcm // invert (one's complement) for negative samples
	}
	if pcm > cClip {
		pcm = cClip
	}
	var seg byte
	switch {
	case pcm < 256:
		seg = 0
	case pcm < 512:
		seg = 1
	case pcm < 1024:
		seg = 2
	case pcm < 2048:
		seg = 3
	case pcm < 4096:
		seg = 4
	case pcm < 8192:
		seg = 5
	case pcm < 16384:
		seg = 6
	default:
		seg = 7
	}
	var mantissa byte
	if seg == 0 {
		mantissa = byte((pcm >> 4) & 0x0F)
	} else {
		mantissa = byte((pcm >> (int(seg) + 3)) & 0x0F)
	}
	return (seg<<4 | mantissa) ^ sign
}
