package main

import (
	"testing"
)

func TestParseHandle(t *testing.T) {
	if h, err := parseHandle("42"); err != nil || h != 42 {
		t.Errorf("parseHandle(42) = %d, %v, want 42, nil", h, err)
	}
	if _, err := parseHandle("not-a-number"); err == nil {
		t.Error("parseHandle(\"not-a-number\") = nil error, want an error")
	}
	if _, err := parseHandle("-1"); err == nil {
		t.Error("parseHandle(\"-1\") = nil error, want an error (unsigned)")
	}
}

func TestTrimHexPrefix(t *testing.T) {
	cases := map[string]string{
		"0x0100b00b51230000": "0100b00b51230000",
		"0X0100B00B51230000": "0100B00B51230000",
		"0100b00b51230000":   "0100b00b51230000",
		"0x":                 "0x",
		"":                   "",
	}
	for in, want := range cases {
		if got := trimHexPrefix(in); got != want {
			t.Errorf("trimHexPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValueOrUnset(t *testing.T) {
	if got := valueOrUnset(""); got != "(unset)" {
		t.Errorf("valueOrUnset(\"\") = %q, want \"(unset)\"", got)
	}
	if got := valueOrUnset("x"); got != "x" {
		t.Errorf("valueOrUnset(\"x\") = %q, want \"x\"", got)
	}
}
