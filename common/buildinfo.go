package common

import "runtime/debug"

// develVersion is the version of a build Go stamped no module version into.
const develVersion = "(devel)"

// readBuildInfo is debug.ReadBuildInfo, a seam for the test.
var readBuildInfo = debug.ReadBuildInfo

// BuildVersion is the version of the running binary's build
// (architecture.md, Components: invocation reference): the module version
// Go stamps into it, the one `go version -m` prints — the tag for a release
// build, a pseudo-version naming the commit for any other build from a
// checkout, with "+dirty" when that checkout had changes — and "(devel)"
// when the build carries none. Every program prints it for --version and
// logs it in its start record; no message carries it.
func BuildVersion() string {
	info, ok := readBuildInfo()
	if !ok || info == nil || info.Main.Version == "" {
		return develVersion
	}
	return info.Main.Version
}
