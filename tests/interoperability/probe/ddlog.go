package main

import (
	"encoding/hex"

	"github.com/pion/rtp/codecs"

	"elnova-streamkit.local/server/sfu/dd"
)

// ddLogger reads the layers of each packet a subscriber receives, as a decoder would: from the dependency descriptor if
// there is one, or else from VP9's own payload descriptor. It keeps one packet's bytes of each kind, for test vectors.
type ddLogger struct {
	structure *dd.Structure
	KeyHex    string
	PlainHex  string
}

func newDDLogger() *ddLogger { return &ddLogger{} }

func (l *ddLogger) parse(ext []byte, rec *packetRecord) {
	rec.hasLayers = true
	rec.extSize = len(ext)
	d, err := dd.Read(ext, l.structure)
	if err != nil {
		rec.ddErr = true
		return
	}
	if d.Attached != nil {
		l.structure = d.Attached
		rec.structure = true
		if l.KeyHex == "" {
			l.KeyHex = hex.EncodeToString(ext)
		}
	} else if l.PlainHex == "" {
		l.PlainHex = hex.EncodeToString(ext)
	}
	rec.spatial = d.Frame.Spatial
	rec.temporal = d.Frame.Temporal
	rec.frame = uint64(d.FrameNumber)
	if d.ActiveTargets != nil && d.Attached == nil {
		rec.mask = int64(*d.ActiveTargets)
	}
}

// parseVP9 is for a stream with no descriptor. A frame is known by its timestamp and spatial layer.
func (l *ddLogger) parseVP9(payload []byte, rec *packetRecord) {
	var p codecs.VP9Packet
	if _, err := p.Unmarshal(payload); err != nil || !p.L {
		return
	}
	rec.hasLayers = true
	rec.spatial = int(p.SID)
	rec.temporal = int(p.TID)
	rec.frame = uint64(rec.ts)<<8 | uint64(p.SID)
}
