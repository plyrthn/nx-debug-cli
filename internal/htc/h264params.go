package htc

// The target's video stream carries nothing but non-IDR slices: no SPS, no
// PPS, ever. That is not a fault in the capture. The parameter sets never
// travel over the wire at all, so a recording taken off the wire has no
// description of how it was encoded and no decoder can open it.
//
// This builds that description instead of shipping a copy of one. The fields
// below are the configuration of the target's encoder, determined by
// comparing candidate parameter sets against a live capture until one parses
// cleanly; what is written here is an ordinary H.264 sequence and picture
// parameter set generated from those numbers, which is what any encoder
// configured this way emits.
//
// Prefixing them to a recording makes it decode: verified with ffmpeg against a
// real capture, where the DevMenu layout comes out at the right geometry.
// Frames still need `-flags2 +showall` to appear at all, because with no IDR
// there is no reference picture to start from - see NXVideoConfig's note.

// H264Config describes an encoder well enough to write its parameter sets.
// Only what this stream needs is modelled: 4:2:0, 8-bit, progressive, one
// reference frame, no scaling matrices.
type H264Config struct {
	ProfileIDC      uint8 // 100 is High
	ConstraintFlags uint8
	LevelIDC        uint8 // level times ten, so 32 is level 3.2

	Width  int
	Height int

	Log2MaxFrameNum       int // as used, not the coded minus-4 form
	PicOrderCntType       int
	MaxNumRefFrames       int
	GapsInFrameNumAllowed bool
	EntropyCodingCAB      bool // CABAC when set, CAVLC when clear

	// Colour description, written as the VUI's video_signal_type. The target
	// tags its stream even though it carries no timing information.
	VideoFormat            int
	ColourPrimaries        uint8
	TransferCharacteristic uint8
	MatrixCoefficients     uint8

	DeblockingFilterControl bool
	Transform8x8Mode        bool
}

// NXVideoConfig is how the devkit encodes `iywys@$remoteVideo`.
//
// Confirmed against a live capture: a Baseline-profile alternative produces
// "illegal reordering_of_pic_nums_idc" on the first slice, this one parses
// every slice cleanly.
//
// What it does not fix: the stream has no IDR and no recovery point, so a
// decoder has nothing to start from and will hold its output back until it
// sees one. Pass `-flags2 +showall` to ffmpeg to get frames out anyway; they
// build up as intra macroblocks arrive rather than snapping into place.
//
// GapsInFrameNumAllowed matters more than it looks: frame_num in a real
// capture cycles 1-14 and starts over, not a steady +1 climb toward this
// SPS's own 256-value modulus, so under the spec's own definition of a gap
// (anything that isn't exactly +1) every wrap reads as one. Left at the
// default false, a strict decoder treats each wrap as an error to recover
// from, and it showed: ffmpeg was only flushing about 1 output picture per
// 13 real ones (a real capture, decoded both ways to compare, went from 32
// output frames to a clean 420 - every single input NAL - once this was set
// true), which is where nearly all of this stream's perceived frame rate
// problem actually was. True tells the decoder the wrap is fine, which,
// given real hardware confirms it happens on every single reference frame,
// is what the encoder's actual behavior calls for.
var NXVideoConfig = H264Config{
	ProfileIDC:              100,
	ConstraintFlags:         0x0c,
	LevelIDC:                32,
	Width:                   1280,
	Height:                  720,
	Log2MaxFrameNum:         8,
	PicOrderCntType:         2,
	MaxNumRefFrames:         1,
	GapsInFrameNumAllowed:   true,
	EntropyCodingCAB:        true,
	VideoFormat:             5, // unspecified
	ColourPrimaries:         1, // BT.709
	TransferCharacteristic:  13,
	MatrixCoefficients:      1, // BT.709
	DeblockingFilterControl: true,
	Transform8x8Mode:        true,
}

// annexBStart is the four-byte start code. The target's own slices use the
// same one, so a prefixed stream stays uniform.
var annexBStart = []byte{0, 0, 0, 1}

// ParameterSets returns the SPS and PPS as one Annex B buffer, ready to write
// in front of a recording.
func (c H264Config) ParameterSets() []byte {
	out := make([]byte, 0, 64)
	out = append(out, annexBStart...)
	out = append(out, c.SPS()...)
	out = append(out, annexBStart...)
	out = append(out, c.PPS()...)
	return out
}

// SPS builds the sequence parameter set, without a start code.
func (c H264Config) SPS() []byte {
	w := &bitWriter{}
	// The NAL header: not a reference picture flag, priority 3, type 7.
	w.byteAligned(0x67)
	w.byteAligned(c.ProfileIDC)
	w.byteAligned(c.ConstraintFlags)
	w.byteAligned(c.LevelIDC)

	w.ue(0) // seq_parameter_set_id

	// The chroma block is only present for the high profiles. Writing it
	// unconditionally is the classic way to produce a parameter set that
	// parses as garbage on Baseline.
	if isHighProfile(c.ProfileIDC) {
		w.ue(1) // chroma_format_idc: 4:2:0
		w.ue(0) // bit_depth_luma_minus8
		w.ue(0) // bit_depth_chroma_minus8
		w.u(0, 1)
		w.u(0, 1) // no scaling matrices
	}

	w.ue(uint(c.Log2MaxFrameNum - 4))
	w.ue(uint(c.PicOrderCntType))
	// Types 0 and 1 carry extra fields. Type 2 derives the order from the
	// frame number, which is why this stream needs none of them.
	switch c.PicOrderCntType {
	case 0:
		w.ue(4)
	case 1:
		w.u(1, 1)
		w.se(0)
		w.se(0)
		w.ue(0)
	}
	w.ue(uint(c.MaxNumRefFrames))
	w.u(boolBit(c.GapsInFrameNumAllowed), 1)

	w.ue(uint(c.Width/16 - 1))
	w.ue(uint(c.Height/16 - 1))
	w.u(1, 1) // frame_mbs_only_flag: progressive
	w.u(1, 1) // direct_8x8_inference_flag
	w.u(0, 1) // frame_cropping_flag: the size is a whole number of macroblocks

	w.u(1, 1) // vui_parameters_present_flag
	w.u(0, 1) // aspect_ratio_info_present_flag
	w.u(0, 1) // overscan_info_present_flag
	w.u(1, 1) // video_signal_type_present_flag
	w.u(uint(c.VideoFormat), 3)
	w.u(0, 1) // video_full_range_flag: limited range
	w.u(1, 1) // colour_description_present_flag
	w.byteAligned(c.ColourPrimaries)
	w.byteAligned(c.TransferCharacteristic)
	w.byteAligned(c.MatrixCoefficients)
	w.u(1, 1) // chroma_loc_info_present_flag
	w.ue(0)   // chroma_sample_loc_type_top_field
	w.ue(0)   // chroma_sample_loc_type_bottom_field
	w.u(0, 1) // timing_info_present_flag: the stream declares no frame rate
	w.u(0, 1) // nal_hrd_parameters_present_flag
	w.u(0, 1) // vcl_hrd_parameters_present_flag
	w.u(0, 1) // pic_struct_present_flag
	w.u(0, 1) // bitstream_restriction_flag

	w.trailing()
	return w.escaped()
}

// PPS builds the picture parameter set, without a start code.
func (c H264Config) PPS() []byte {
	w := &bitWriter{}
	w.byteAligned(0x68) // priority 3, type 8

	w.ue(0) // pic_parameter_set_id
	w.ue(0) // seq_parameter_set_id
	w.u(boolBit(c.EntropyCodingCAB), 1)
	w.u(0, 1) // bottom_field_pic_order_in_frame_present_flag
	w.ue(0)   // num_slice_groups_minus1
	w.ue(0)   // num_ref_idx_l0_default_active_minus1
	w.ue(0)   // num_ref_idx_l1_default_active_minus1
	w.u(0, 1) // weighted_pred_flag
	w.u(0, 2) // weighted_bipred_idc
	w.se(0)   // pic_init_qp_minus26
	w.se(0)   // pic_init_qs_minus26
	w.se(0)   // chroma_qp_index_offset
	w.u(boolBit(c.DeblockingFilterControl), 1)
	// constrained_intra_pred_flag: confirmed 0 against a real gameplay
	// capture - forcing this to 1 makes ffmpeg reject intra macroblocks
	// with "block unavailable for requested intra mode", which the real
	// stream never triggers at 0.
	w.u(0, 1)
	w.u(0, 1) // redundant_pic_cnt_present_flag

	// The 8x8 transform belongs to the high profiles and its presence is
	// signalled by the parameter set simply continuing, not by a flag.
	if c.Transform8x8Mode {
		w.u(1, 1)
		w.u(0, 1) // pic_scaling_matrix_present_flag
		w.se(0)   // second_chroma_qp_index_offset
	}

	w.trailing()
	return w.escaped()
}

func isHighProfile(profile uint8) bool {
	switch profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		return true
	}
	return false
}

func boolBit(b bool) uint {
	if b {
		return 1
	}
	return 0
}

// bitWriter writes the bit-packed syntax H.264 parameter sets are made of.
type bitWriter struct {
	buf  []byte
	bits int // bits used in the last byte
}

func (w *bitWriter) u(v uint, n int) {
	for i := n - 1; i >= 0; i-- {
		if w.bits == 0 {
			w.buf = append(w.buf, 0)
			w.bits = 8
		}
		w.bits--
		if (v>>uint(i))&1 != 0 {
			w.buf[len(w.buf)-1] |= 1 << uint(w.bits)
		}
	}
}

// byteAligned writes a whole byte. It goes through u so a caller cannot
// accidentally write one while the stream is mid-byte and silently shift
// everything after it.
func (w *bitWriter) byteAligned(v uint8) { w.u(uint(v), 8) }

// ue writes an unsigned Exp-Golomb code: the value plus one in binary, with
// that many minus one leading zeroes in front of it.
func (w *bitWriter) ue(v uint) {
	v++
	n := 0
	for x := v; x > 1; x >>= 1 {
		n++
	}
	w.u(0, n)
	w.u(v, n+1)
}

// se writes a signed Exp-Golomb code, which is the unsigned one over an
// alternating positive/negative mapping.
func (w *bitWriter) se(v int) {
	if v > 0 {
		w.ue(uint(2*v - 1))
		return
	}
	w.ue(uint(-2 * v))
}

// trailing closes the RBSP: a stop bit, then zeroes to the byte boundary.
func (w *bitWriter) trailing() {
	w.u(1, 1)
	if w.bits > 0 {
		w.u(0, w.bits)
	}
}

// escaped returns the bytes with emulation prevention applied. Any run of two
// zero bytes followed by a value under four has a 0x03 inserted, so the result
// cannot contain something a decoder would read as a start code.
func (w *bitWriter) escaped() []byte {
	out := make([]byte, 0, len(w.buf)+4)
	zeroes := 0
	for i, b := range w.buf {
		// The NAL header byte is not part of the RBSP and is never escaped.
		if i > 0 && zeroes >= 2 && b <= 3 {
			out = append(out, 0x03)
			zeroes = 0
		}
		out = append(out, b)
		if b == 0 {
			zeroes++
		} else {
			zeroes = 0
		}
	}
	return out
}
