package dd

import "errors"

var errShortBuffer = errors.New("dependency descriptor is shorter than its fields")

// bitReader reads big-endian bit fields, the f(n) of the specification.
type bitReader struct {
	data []byte
	pos  int // in bits
}

func (r *bitReader) bits(n int) (uint32, error) {
	if r.pos+n > len(r.data)*8 {
		return 0, errShortBuffer
	}
	var v uint32
	for range n {
		v = v<<1 | uint32(r.data[r.pos/8]>>(7-r.pos%8))&1
		r.pos++
	}
	return v, nil
}

func (r *bitReader) flag() (bool, error) {
	v, err := r.bits(1)
	return v == 1, err
}

// nonSymmetric reads the ns(n) format: values below a threshold take one bit fewer.
func (r *bitReader) nonSymmetric(n uint32) (uint32, error) {
	w := bitLength(n)
	m := uint32(1)<<w - n
	v, err := r.bits(w - 1)
	if err != nil || v < m {
		return v, err
	}
	extra, err := r.bits(1)
	if err != nil {
		return 0, err
	}
	return v<<1 - m + extra, nil
}

// bitWriter is the other direction. It grows as needed, reusing the slice it starts with.
type bitWriter struct {
	data []byte
	pos  int
}

func (w *bitWriter) bits(v uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		if w.pos/8 == len(w.data) {
			w.data = append(w.data, 0)
		}
		w.data[w.pos/8] |= byte(v>>i&1) << (7 - w.pos%8)
		w.pos++
	}
}

func (w *bitWriter) flag(v bool) {
	if v {
		w.bits(1, 1)
	} else {
		w.bits(0, 1)
	}
}

func (w *bitWriter) nonSymmetric(v, n uint32) {
	width := bitLength(n)
	m := uint32(1)<<width - n
	if v < m {
		w.bits(v, width-1)
		return
	}
	w.bits(v+m, width)
}

// bitLength is floor(log2(n)) + 1.
func bitLength(n uint32) int {
	w := 0
	for n != 0 {
		n >>= 1
		w++
	}
	return w
}
