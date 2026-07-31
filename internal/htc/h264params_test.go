package htc

import (
	"encoding/hex"
	"testing"
)

// bitReader is the inverse of bitWriter, so the generated sets can be checked
// by reading them back rather than by comparing against a second copy of the
// same constants.
type bitReader struct {
	b []byte
	p int
}

func (r *bitReader) u(n int) uint {
	var v uint
	for i := 0; i < n; i++ {
		v = v<<1 | uint(r.b[r.p>>3]>>(7-uint(r.p&7))&1)
		r.p++
	}
	return v
}

func (r *bitReader) ue() uint {
	z := 0
	for r.u(1) == 0 {
		z++
	}
	if z == 0 {
		return 0
	}
	return 1<<uint(z) - 1 + r.u(z)
}

func (r *bitReader) se() int {
	k := r.ue()
	if k%2 == 1 {
		return int(k+1) / 2
	}
	return -int(k / 2)
}

// unescape removes the emulation prevention bytes, which is what a decoder
// does before reading any of the syntax.
func unescape(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if i+2 < len(b) && b[i] == 0 && b[i+1] == 0 && b[i+2] == 3 {
			out = append(out, 0, 0)
			i += 2
			continue
		}
		out = append(out, b[i])
	}
	return out
}

func TestSPSReadsBackAsWritten(t *testing.T) {
	sps := NXVideoConfig.SPS()
	if got := sps[0] & 0x1f; got != 7 {
		t.Fatalf("NAL type = %d, want 7 (SPS)", got)
	}
	if sps[1] != NXVideoConfig.ProfileIDC || sps[2] != NXVideoConfig.ConstraintFlags || sps[3] != NXVideoConfig.LevelIDC {
		t.Fatalf("profile/constraints/level = %d/%d/%d, want %d/%d/%d",
			sps[1], sps[2], sps[3], NXVideoConfig.ProfileIDC, NXVideoConfig.ConstraintFlags, NXVideoConfig.LevelIDC)
	}

	r := &bitReader{b: unescape(sps[4:])}
	if got := r.ue(); got != 0 {
		t.Errorf("seq_parameter_set_id = %d, want 0", got)
	}
	if got := r.ue(); got != 1 {
		t.Errorf("chroma_format_idc = %d, want 1 (4:2:0)", got)
	}
	if got := r.ue(); got != 0 {
		t.Errorf("bit_depth_luma_minus8 = %d, want 0", got)
	}
	if got := r.ue(); got != 0 {
		t.Errorf("bit_depth_chroma_minus8 = %d, want 0", got)
	}
	r.u(1) // qpprime_y_zero_transform_bypass_flag
	if got := r.u(1); got != 0 {
		t.Errorf("seq_scaling_matrix_present_flag = %d, want 0", got)
	}
	if got := int(r.ue()) + 4; got != NXVideoConfig.Log2MaxFrameNum {
		t.Errorf("log2_max_frame_num = %d, want %d", got, NXVideoConfig.Log2MaxFrameNum)
	}
	if got := int(r.ue()); got != NXVideoConfig.PicOrderCntType {
		t.Errorf("pic_order_cnt_type = %d, want %d", got, NXVideoConfig.PicOrderCntType)
	}
	if got := int(r.ue()); got != NXVideoConfig.MaxNumRefFrames {
		t.Errorf("max_num_ref_frames = %d, want %d", got, NXVideoConfig.MaxNumRefFrames)
	}
	if got := r.u(1) == 1; got != NXVideoConfig.GapsInFrameNumAllowed {
		t.Errorf("gaps_in_frame_num_value_allowed_flag = %v, want %v", got, NXVideoConfig.GapsInFrameNumAllowed)
	}
	if got := (int(r.ue()) + 1) * 16; got != NXVideoConfig.Width {
		t.Errorf("width = %d, want %d", got, NXVideoConfig.Width)
	}
	if got := (int(r.ue()) + 1) * 16; got != NXVideoConfig.Height {
		t.Errorf("height = %d, want %d", got, NXVideoConfig.Height)
	}
	if got := r.u(1); got != 1 {
		t.Errorf("frame_mbs_only_flag = %d, want 1", got)
	}
	r.u(1) // direct_8x8_inference_flag
	if got := r.u(1); got != 0 {
		t.Errorf("frame_cropping_flag = %d, want 0", got)
	}
	if got := r.u(1); got != 1 {
		t.Fatalf("vui_parameters_present_flag = %d, want 1", got)
	}

	// The colour description is the part of the VUI the target actually sets.
	if got := r.u(1); got != 0 {
		t.Errorf("aspect_ratio_info_present_flag = %d, want 0", got)
	}
	if got := r.u(1); got != 0 {
		t.Errorf("overscan_info_present_flag = %d, want 0", got)
	}
	if got := r.u(1); got != 1 {
		t.Fatalf("video_signal_type_present_flag = %d, want 1", got)
	}
	if got := int(r.u(3)); got != NXVideoConfig.VideoFormat {
		t.Errorf("video_format = %d, want %d", got, NXVideoConfig.VideoFormat)
	}
	r.u(1) // video_full_range_flag
	if got := r.u(1); got != 1 {
		t.Fatalf("colour_description_present_flag = %d, want 1", got)
	}
	if got := uint8(r.u(8)); got != NXVideoConfig.ColourPrimaries {
		t.Errorf("colour_primaries = %d, want %d", got, NXVideoConfig.ColourPrimaries)
	}
	if got := uint8(r.u(8)); got != NXVideoConfig.TransferCharacteristic {
		t.Errorf("transfer_characteristics = %d, want %d", got, NXVideoConfig.TransferCharacteristic)
	}
	if got := uint8(r.u(8)); got != NXVideoConfig.MatrixCoefficients {
		t.Errorf("matrix_coefficients = %d, want %d", got, NXVideoConfig.MatrixCoefficients)
	}
}

func TestPPSReadsBackAsWritten(t *testing.T) {
	pps := NXVideoConfig.PPS()
	if got := pps[0] & 0x1f; got != 8 {
		t.Fatalf("NAL type = %d, want 8 (PPS)", got)
	}
	r := &bitReader{b: unescape(pps[1:])}
	if got := r.ue(); got != 0 {
		t.Errorf("pic_parameter_set_id = %d, want 0", got)
	}
	if got := r.ue(); got != 0 {
		t.Errorf("seq_parameter_set_id = %d, want 0", got)
	}
	if got := r.u(1); got != 1 {
		t.Errorf("entropy_coding_mode_flag = %d, want 1 (CABAC)", got)
	}
	r.u(1) // bottom_field_pic_order_in_frame_present_flag
	if got := r.ue(); got != 0 {
		t.Errorf("num_slice_groups_minus1 = %d, want 0", got)
	}
	r.ue()
	r.ue()
	r.u(1)
	r.u(2)
	if got := r.se(); got != 0 {
		t.Errorf("pic_init_qp_minus26 = %d, want 0", got)
	}
	r.se()
	if got := r.se(); got != 0 {
		t.Errorf("chroma_qp_index_offset = %d, want 0", got)
	}
	if got := r.u(1); got != 1 {
		t.Errorf("deblocking_filter_control_present_flag = %d, want 1", got)
	}
	r.u(1)
	r.u(1)
	if got := r.u(1); got != 1 {
		t.Errorf("transform_8x8_mode_flag = %d, want 1", got)
	}
}

// The generated sets have to be the ones the target's encoder was configured
// with, byte for byte. Anything else parses as a different encoder and decodes
// to confident garbage, which is worse than not decoding at all.
func TestParameterSetsMatchTheTargetEncoder(t *testing.T) {
	cases := []struct {
		name string
		got  []byte
		want string
	}{
		{"SPS", NXVideoConfig.SPS(), "67640c20ac2b502802dd35010d01e080"},
		{"PPS", NXVideoConfig.PPS(), "68ee3cb0"},
	}
	for _, c := range cases {
		if got := hex.EncodeToString(c.got); got != c.want {
			t.Errorf("%s = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestParameterSetsCarryStartCodes(t *testing.T) {
	b := NXVideoConfig.ParameterSets()
	sps, pps := NXVideoConfig.SPS(), NXVideoConfig.PPS()
	want := len(annexBStart)*2 + len(sps) + len(pps)
	if len(b) != want {
		t.Fatalf("parameter sets are %d bytes, want %d", len(b), want)
	}
	if b[0] != 0 || b[1] != 0 || b[2] != 0 || b[3] != 1 {
		t.Errorf("no start code in front of the SPS: % x", b[:4])
	}
	at := len(annexBStart) + len(sps)
	if b[at] != 0 || b[at+1] != 0 || b[at+2] != 0 || b[at+3] != 1 {
		t.Errorf("no start code in front of the PPS: % x", b[at:at+4])
	}
}

// Emulation prevention is what stops a parameter set from containing something
// a decoder reads as the start of the next unit.
func TestEscapingBreaksUpStartCodes(t *testing.T) {
	w := &bitWriter{}
	w.byteAligned(0x67)
	w.byteAligned(0)
	w.byteAligned(0)
	w.byteAligned(1)
	got := w.escaped()
	want := []byte{0x67, 0, 0, 3, 1}
	if len(got) != len(want) {
		t.Fatalf("escaped = % x, want % x", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("escaped = % x, want % x", got, want)
		}
	}
}

func TestExpGolomb(t *testing.T) {
	for _, v := range []uint{0, 1, 2, 3, 7, 8, 79, 44, 1000} {
		w := &bitWriter{}
		w.ue(v)
		w.trailing()
		if got := (&bitReader{b: w.buf}).ue(); got != v {
			t.Errorf("ue(%d) read back as %d", v, got)
		}
	}
	for _, v := range []int{0, 1, -1, 2, -2, 13, -13} {
		w := &bitWriter{}
		w.se(v)
		w.trailing()
		if got := (&bitReader{b: w.buf}).se(); got != v {
			t.Errorf("se(%d) read back as %d", v, got)
		}
	}
}

// A parameter set that skipped the high-profile chroma block would still parse
// on a Baseline decoder and misplace every field after it.
func TestBaselineProfileOmitsTheChromaBlock(t *testing.T) {
	base := NXVideoConfig
	base.ProfileIDC = 66
	base.Transform8x8Mode = false
	sps := base.SPS()

	r := &bitReader{b: unescape(sps[4:])}
	r.ue() // seq_parameter_set_id
	if got := int(r.ue()) + 4; got != base.Log2MaxFrameNum {
		t.Errorf("log2_max_frame_num = %d, want %d: the chroma block was written for Baseline", got, base.Log2MaxFrameNum)
	}
}
