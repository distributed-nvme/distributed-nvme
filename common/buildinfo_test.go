package common

import (
	"runtime/debug"
	"testing"
)

// TestBuildVersion pins what every program prints for --version and logs in
// its start record (architecture.md, Components: invocation reference): the
// module version Go stamps into the binary, as `go version -m` prints it, and
// "(devel)" when the build carries none.
func TestBuildVersion(t *testing.T) {
	defer func(read func() (*debug.BuildInfo, bool)) { readBuildInfo = read }(readBuildInfo)
	for _, tc := range []struct {
		name string
		info *debug.BuildInfo
		ok   bool
		want string
	}{
		{"a release", &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}}, true, "v0.1.0"},
		{"a later commit with changes", &debug.BuildInfo{Main: debug.Module{
			Version: "v0.1.1-0.20261119072047-0259e830adf7+dirty"}}, true,
			"v0.1.1-0.20261119072047-0259e830adf7+dirty"},
		{"a build with no stamp", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, true, "(devel)"},
		{"an empty version", &debug.BuildInfo{}, true, "(devel)"},
		{"no build information", nil, false, "(devel)"},
	} {
		readBuildInfo = func() (*debug.BuildInfo, bool) { return tc.info, tc.ok }
		if got := BuildVersion(); got != tc.want {
			t.Errorf("%s: BuildVersion() = %q, want %q", tc.name, got, tc.want)
		}
	}
}
