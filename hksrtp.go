// SRTP forwarding of a camera's H.264 RTP stream to a HomeKit controller.
//
// HomeKit negotiates in two steps. SetupEndpoints tells us where to send
// and hands over the SRTP master key and salt the accessory must encrypt
// video with. SelectedRTPStreamConfiguration then pins the SSRC, payload
// type and MTU. Everything in this file is that translation: take the RTP
// packets rtsp.go already produces, restamp them onto the negotiated
// identity, encrypt, and put them on the wire.
//
// No transcoding happens here and none is wanted — the Dahua VTOs emit
// H.264, which is exactly what HomeKit takes, so the packets pass through
// with only their headers rewritten. That is what lets camonitor stay a
// single static binary with no ffmpeg.
package main

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"sync"

	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
)

// srtpForwarder implements trackWriter, so it can be handed straight to
// runRTSPReader in place of a WebRTC track.
type srtpForwarder struct {
	conn        *net.UDPConn
	target      *net.UDPAddr
	streamID    string
	ssrc        uint32
	payloadType uint8
	mtu         int

	mu        sync.Mutex
	ctx       *srtp.Context
	seq       uint16
	closed    bool
	warnedMTU bool
}

// newSRTPForwarder prepares the encryption context around an already-bound
// socket. key and salt come from the controller's SetupEndpoints request.
//
// The socket is bound during SetupEndpoints rather than here, because its
// port is part of the answer the controller is given — it addresses its
// RTCP to it. Binding at stream-start would mean advertising a port we had
// not chosen yet.
func newSRTPForwarder(streamID string, conn *net.UDPConn, target *net.UDPAddr, key, salt []byte, ssrc uint32, payloadType uint8, mtu int) (*srtpForwarder, error) {
	ctx, err := srtp.CreateContext(key, salt, srtp.ProtectionProfileAes128CmHmacSha1_80)
	if err != nil {
		return nil, fmt.Errorf("srtp context: %w", err)
	}

	return &srtpForwarder{
		conn:        conn,
		target:      target,
		streamID:    streamID,
		ssrc:        ssrc,
		payloadType: payloadType,
		mtu:         mtu,
		ctx:         ctx,
		seq:         randomUint16(),
	}, nil
}

// WriteRTP restamps, encrypts and sends one packet.
//
// The camera's own SSRC, payload type and sequence numbering mean nothing
// to HomeKit — it rejects anything that doesn't match what it negotiated —
// and SRTP's replay window needs a sequence space we control rather than
// one that jumps when the camera reconnects. So all three are rewritten.
// The timestamp is passed through: RTSP H.264 and HomeKit both clock video
// at 90 kHz.
func (f *srtpForwarder) WriteRTP(p *rtp.Packet) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return nil
	}

	pkt := *p
	pkt.Header.SSRC = f.ssrc
	pkt.Header.PayloadType = f.payloadType
	pkt.Header.SequenceNumber = f.seq
	f.seq++

	raw, err := pkt.Marshal()
	if err != nil {
		return fmt.Errorf("marshal rtp: %w", err)
	}

	// The camera packetises to its own MTU. If it ever exceeds what
	// HomeKit asked for, say so once — re-fragmenting H.264 FU-A units
	// here would be the fix, and silence would make it look like a
	// network problem instead.
	if f.mtu > 0 && len(raw) > f.mtu && !f.warnedMTU {
		f.warnedMTU = true
		log.Printf("homekit camera [%s]: camera packet %d bytes exceeds negotiated MTU %d; "+
			"if the stream stutters, this is why", f.streamID, len(raw), f.mtu)
	}

	encrypted, err := f.ctx.EncryptRTP(nil, raw, nil)
	if err != nil {
		return fmt.Errorf("encrypt rtp: %w", err)
	}
	if _, err := f.conn.WriteToUDP(encrypted, f.target); err != nil {
		return fmt.Errorf("send to controller: %w", err)
	}
	return nil
}

func (f *srtpForwarder) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	return f.conn.Close()
}

func randomUint16() uint16 {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint16(b[:])
}

func randomUint32() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}

// randomSRTPKeySalt returns a fresh AES_CM_128_HMAC_SHA1_80 master key and
// salt, used for the accessory's half of the SetupEndpoints exchange.
func randomSRTPKeySalt() (key, salt []byte) {
	key = make([]byte, 16)
	salt = make([]byte, 14)
	_, _ = rand.Read(key)
	_, _ = rand.Read(salt)
	return key, salt
}
