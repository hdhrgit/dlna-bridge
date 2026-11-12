package cut

import (
	"bytes"
	"errors"
)

// VideoFormat reads the picture size and frame rate of the H.264 video in the
// start of a transport stream, from its first sequence parameter set. ok is
// false until b holds the PAT, the PMT and the whole SPS, or when the video is
// not H.264. fps is 0 when the SPS does not say.
func VideoFormat(b []byte) (w, h int, fps float64, ok bool) {
	s, err := findStreams(b)
	if err != nil || s.codec != 0x1b {
		return 0, 0, 0, false
	}
	var es []byte
	for off := sync(b); off+packetSize <= len(b); off += packetSize {
		pk, ok := parsePacket(b[off : off+packetSize])
		if !ok || pk.pid != s.videoPID {
			continue
		}
		p := pk.payload
		if pk.pusi {
			if len(p) < 9 || 9+int(p[8]) > len(p) {
				continue
			}
			p = p[9+int(p[8]):]
		}
		es = append(es, p...)
	}
	for {
		i := bytes.Index(es, []byte{0, 0, 1})
		if i < 0 || i+4 > len(es) {
			return 0, 0, 0, false
		}
		es = es[i+3:]
		if es[0]&0x1f != 7 {
			continue
		}
		end := bytes.Index(es, []byte{0, 0, 1})
		if end < 0 {
			return 0, 0, 0, false // the rest of it is still to come
		}
		w, h, fps, err := parseSPS(es[1:end])
		return w, h, fps, err == nil
	}
}

var errShort = errors.New("SPS cut short")

type bitReader struct {
	b   []byte
	pos int // in bits
	err error
}

func (r *bitReader) bit() uint {
	if r.pos >= 8*len(r.b) {
		r.err = errShort
		return 0
	}
	v := uint(r.b[r.pos/8]>>(7-r.pos%8)) & 1
	r.pos++
	return v
}

func (r *bitReader) bits(n int) uint {
	var v uint
	for range n {
		v = v<<1 | r.bit()
	}
	return v
}

// ue reads an unsigned Exp-Golomb number.
func (r *bitReader) ue() uint {
	zeros := 0
	for r.bit() == 0 && r.err == nil && zeros < 32 {
		zeros++
	}
	return 1<<zeros - 1 + r.bits(zeros)
}

func (r *bitReader) se() int {
	v := r.ue()
	if v&1 != 0 {
		return int(v+1) / 2
	}
	return -int(v / 2)
}

// parseSPS reads the picture size, less its cropping, and the frame rate from
// the timing in its VUI, from an SPS NAL unit's payload (after its header).
func parseSPS(nal []byte) (w, h int, fps float64, err error) {
	// The encoder puts 3 after each 0 0 that would otherwise look like a start code.
	rbsp := make([]byte, 0, len(nal))
	zeros := 0
	for _, c := range nal {
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
		rbsp = append(rbsp, c)
	}
	r := &bitReader{b: rbsp}
	profile := r.bits(8)
	r.bits(16) // constraint flags, level
	r.ue()     // seq_parameter_set_id
	chroma := uint(1)
	switch profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		if chroma = r.ue(); chroma == 3 {
			r.bit() // separate_colour_plane_flag
		}
		r.ue()            // bit_depth_luma
		r.ue()            // bit_depth_chroma
		r.bit()           // qpprime_y_zero_transform_bypass
		if r.bit() != 0 { // seq_scaling_matrix_present
			n := 8
			if chroma == 3 {
				n = 12
			}
			for i := range n {
				if r.bit() == 0 {
					continue
				}
				size := 16
				if i >= 6 {
					size = 64
				}
				last, next := 8, 8
				for range size {
					if next != 0 {
						next = (last + r.se() + 256) % 256
					}
					if next != 0 {
						last = next
					}
				}
			}
		}
	}
	r.ue()          // log2_max_frame_num
	switch r.ue() { // pic_order_cnt_type
	case 0:
		r.ue()
	case 1:
		r.bit()
		r.se()
		r.se()
		for n := r.ue(); n > 0 && r.err == nil; n-- {
			r.se()
		}
	}
	r.ue()  // max_num_ref_frames
	r.bit() // gaps_in_frame_num_allowed
	mbW := int(r.ue()) + 1
	mapH := int(r.ue()) + 1
	frameMbsOnly := int(r.bit())
	if frameMbsOnly == 0 {
		r.bit() // mb_adaptive_frame_field
	}
	r.bit() // direct_8x8_inference
	w, h = mbW*16, (2-frameMbsOnly)*mapH*16
	if r.bit() != 0 { // frame_cropping
		cx, cy := 1, 2-frameMbsOnly
		if chroma == 1 || chroma == 2 {
			cx = 2
		}
		if chroma == 1 {
			cy *= 2
		}
		l, rt, t, bm := int(r.ue()), int(r.ue()), int(r.ue()), int(r.ue())
		w -= cx * (l + rt)
		h -= cy * (t + bm)
	}
	if r.bit() != 0 { // vui_parameters_present
		if r.bit() != 0 { // aspect_ratio_info
			if r.bits(8) == 255 {
				r.bits(32)
			}
		}
		if r.bit() != 0 { // overscan_info
			r.bit()
		}
		if r.bit() != 0 { // video_signal_type
			r.bits(4)
			if r.bit() != 0 {
				r.bits(24)
			}
		}
		if r.bit() != 0 { // chroma_loc_info
			r.ue()
			r.ue()
		}
		if r.bit() != 0 { // timing_info
			tick, scale := r.bits(32), r.bits(32)
			if tick > 0 {
				fps = float64(scale) / float64(2*tick)
			}
		}
	}
	if r.err != nil || w <= 0 || h <= 0 {
		return 0, 0, 0, errShort
	}
	return w, h, fps, nil
}
