package sfu

// vp8Descriptor is the parsed form of an RTP payload descriptor (RFC 7741, section 4.2).
type vp8Descriptor struct {
	start       bool
	partitionID uint8
	headerLen   int

	hasPicID  bool
	picIDWide bool // 15-bit form
	picIDAt   int
	picID     uint16

	hasTL0   bool
	tl0At    int
	tl0PicID uint8

	// hasTID says the frame states its temporal layer. sync means no frame after this one depends on a frame of this
	// layer from before it, so a receiver that was not getting this layer can start here.
	hasTID bool
	tid    uint8
	sync   bool
}

func parseVP8Descriptor(p []byte) (vp8Descriptor, bool) {
	var d vp8Descriptor
	if len(p) < 1 {
		return d, false
	}
	b0 := p[0]
	d.start = b0&0x10 != 0
	d.partitionID = b0 & 0x07
	i := 1
	if b0&0x80 != 0 { // X: extension byte follows
		if i >= len(p) {
			return d, false
		}
		x := p[i]
		i++
		if x&0x80 != 0 { // I: picture ID
			if i >= len(p) {
				return d, false
			}
			d.hasPicID = true
			d.picIDAt = i
			if p[i]&0x80 != 0 {
				if i+1 >= len(p) {
					return d, false
				}
				d.picIDWide = true
				d.picID = uint16(p[i]&0x7f)<<8 | uint16(p[i+1])
				i += 2
			} else {
				d.picID = uint16(p[i])
				i++
			}
		}
		if x&0x40 != 0 { // L: TL0PICIDX
			if i >= len(p) {
				return d, false
			}
			d.hasTL0 = true
			d.tl0At = i
			d.tl0PicID = p[i]
			i++
		}
		if x&0x30 != 0 { // T or K: TID/KEYIDX byte
			if i >= len(p) {
				return d, false
			}
			if x&0x20 != 0 {
				d.hasTID = true
				d.tid = p[i] >> 6
				d.sync = p[i]&0x20 != 0
			}
			i++
		}
	}
	d.headerLen = i
	return d, true
}

// vp8Munger keeps picture IDs and TL0PICIDX continuous across simulcast layer switches, because each
// layer numbers its frames independently and receivers use these values to track frame dependencies.
type vp8Munger struct {
	picOffset  uint16
	tl0Offset  uint8
	lastPic    uint16
	lastTL0    uint8
	started    bool
	skippedPic uint16
	skipping   bool
}

// skip notes a frame that will not be forwarded. Chrome waits for a frame whose picture ID it has seen a gap for, so the IDs
// have to close up over what was dropped.
func (m *vp8Munger) skip(d vp8Descriptor) {
	if !d.hasPicID || (m.skipping && m.skippedPic == d.picID) {
		return
	}
	m.skippedPic, m.skipping = d.picID, true
	m.picOffset = (m.picOffset + 1) & 0x7fff
}

// onSwitch rebases the offsets so the next forwarded frame, carrying d, directly follows the last one.
func (m *vp8Munger) onSwitch(d vp8Descriptor) {
	if !m.started {
		return
	}
	if d.hasPicID {
		m.picOffset = (d.picID - (m.lastPic + 1)) & 0x7fff
	}
	if d.hasTL0 {
		m.tl0Offset = d.tl0PicID - (m.lastTL0 + 1)
	}
}

// apply rewrites p in place. p must be a private copy when an offset is nonzero; see needsCopy.
func (m *vp8Munger) apply(p []byte, d vp8Descriptor) {
	if d.hasPicID {
		pic := (d.picID - m.picOffset) & 0x7fff
		if d.picIDWide {
			p[d.picIDAt] = 0x80 | byte(pic>>8)
			p[d.picIDAt+1] = byte(pic)
		} else {
			// The narrow form carries 7 bits.
			pic &= 0x7f
			p[d.picIDAt] = byte(pic)
		}
		m.lastPic = pic
	}
	if d.hasTL0 {
		tl0 := d.tl0PicID - m.tl0Offset
		p[d.tl0At] = tl0
		m.lastTL0 = tl0
	}
	m.started = true
}

func (m *vp8Munger) needsCopy() bool { return m.picOffset != 0 || m.tl0Offset != 0 }
