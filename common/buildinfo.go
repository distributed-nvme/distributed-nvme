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
// checkout whose .git is a directory, with "+dirty" when that checkout had
// changes — and "(devel)" when the build carries none. Go takes the version
// from the nearest directory, at or above the module's, whose .git is a
// directory, so a build in a git worktree, whose .git is a file, carries the
// commit of the checkout whose folder holds the worktree, when one does, and
// else none. Every
// program prints it for --version, every daemon logs it in its start
// record, and the CSI identity service reports it as the driver's vendor
// version (csi.Version); no message between dnv programs carries it.
func BuildVersion() string {
	info, ok := readBuildInfo()
	if !ok || info == nil || info.Main.Version == "" {
		return develVersion
	}
	return info.Main.Version
}
