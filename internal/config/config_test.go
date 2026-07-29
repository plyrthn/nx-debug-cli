package config

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  Config
	}{
		{"empty", "", Config{}},
		{
			"a field",
			"output_dir = /tmp/nxdbg\n",
			Config{OutputDir: "/tmp/nxdbg"},
		},
		{
			"comments and blank lines ignored",
			"# a comment\n\noutput_dir = /tmp/nxdbg\n\n# trailing\n",
			Config{OutputDir: "/tmp/nxdbg"},
		},
		{
			"surrounding whitespace trimmed",
			"  output_dir   =   /tmp/nxdbg   \n",
			Config{OutputDir: "/tmp/nxdbg"},
		},
		{
			"unknown key ignored",
			"nonsense = whatever\noutput_dir = /tmp/nxdbg\n",
			Config{OutputDir: "/tmp/nxdbg"},
		},
		{
			"line with no '=' ignored",
			"this is not valid\noutput_dir = /tmp/nxdbg\n",
			Config{OutputDir: "/tmp/nxdbg"},
		},
		{
			"value containing '=' keeps the rest",
			"output_dir = C:\\path=with=equals\n",
			Config{OutputDir: `C:\path=with=equals`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parse(strings.NewReader(tc.input))
			if err != nil {
				t.Fatalf("parse() error = %v", err)
			}
			if got != tc.want {
				t.Errorf("parse() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestPath(t *testing.T) {
	path, err := Path()
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}
	if !strings.Contains(path, "nx-debug-cli") {
		t.Errorf("Path() = %q, want it to contain \"nx-debug-cli\"", path)
	}
	if !strings.HasSuffix(path, "config") {
		t.Errorf("Path() = %q, want it to end with \"config\"", path)
	}
}

func TestLoadMissingFileReturnsZeroValue(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg != (Config{}) {
		t.Errorf("Load() with no config file = %+v, want zero value", cfg)
	}
}
