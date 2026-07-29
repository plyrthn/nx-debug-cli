package htc

import "sort"

// Service is a well-known HTCS port on a target. Every channel this tool
// knows how to open is declared here once, so a new one is a single entry
// rather than a string literal at some call site plus a switch somewhere
// else that has to be remembered.
//
// The "iywys@$" prefix is Nintendo's system-wide namespace for these; the
// same convention shows up in Atmosphere's htc reimplementation
// ("iywys@$gdb", "iywys@$cs", "iywys@$LogManager"), so it isn't specific to
// any one service. A few older ports predate the convention and have bare
// names.
type Service struct {
	// Key is the short name used on the command line and in the TUI.
	Key string
	// Port is the HTCS port name exactly as it appears in the port map.
	Port string
	// Desc is a one-line explanation for listings.
	Desc string
}

// serviceList is the registry, built from ports observed live on an EDEV. A
// target only publishes the subset its running software actually listens on.
var serviceList = []Service{
	{"hid", "iywys@$hid", "remote input injection (mouse, touch, keyboard)"},
	{"video", "iywys@$remoteVideo", "remote video stream"},
	{"video-shell", "iywys@$remoteVideoShell", "remote video control shell"},
	{"audio", "iywys@$remoteAudio", "remote audio stream"},
	{"audio-capture", "iywys@$audio", "audio capture"},
	{"gdb", "iywys@$gdb", "GDB stub"},
	{"dmnt", "iywys@$dmnt", "debug monitor"},
	{"dmnt-log", "iywys@$dmnt_log", "debug monitor log"},
	{"cs", "iywys@$cs", "command shell"},
	{"cs-runner", "iywys@$csForRunnerTools", "command shell, test runner instance"},
	{"log-manager", "iywys@$LogManager", "structured log manager"},
	{"log-json", "iywys@$JsonLog", "JSON log stream"},
	{"log-hashed", "iywys@$HashedLog", "hashed log stream"},
	{"perfmon", "iywys@$perfmon", "performance monitor"},
	{"fileserver", "iywys@$TioServer_FileServer", "host file system server"},
	{"log", "@Log", "plain target log"},
	{"log-json-legacy", "@JsonLog", "JSON target log (pre-iywys naming)"},
	{"cpu-profiler", "NintendoCpuProfiler:out", "CPU profiler output"},
	{"nvdbgsvc", "nvdbgsvc", "NVIDIA graphics debugger"},
}

var (
	servicesByKey  = map[string]Service{}
	servicesByPort = map[string]Service{}
)

func init() {
	for _, s := range serviceList {
		servicesByKey[s.Key] = s
		servicesByPort[s.Port] = s
	}
}

// Services returns the registry sorted by key.
func Services() []Service {
	out := append([]Service(nil), serviceList...)
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// LookupService resolves a short key. It also accepts a full HTCS port name,
// so a caller can pass either without having to know which it has.
func LookupService(name string) (Service, bool) {
	if s, ok := servicesByKey[name]; ok {
		return s, true
	}
	s, ok := servicesByPort[name]
	return s, ok
}

// ServiceForPort maps an HTCS port name back to its registry entry. An
// unrecognised port reports false rather than being guessed at - targets can
// publish ports this build has never heard of, and labelling one of those as
// some unrelated known service would be worse than saying nothing.
func ServiceForPort(port string) (Service, bool) {
	s, ok := servicesByPort[port]
	return s, ok
}
