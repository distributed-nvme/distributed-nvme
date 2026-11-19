package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// The version reads an agent's start record and the node check share
// (dnagent.md CM4, CM5; cnagent.md CN-CM2): the kernel release and mdadm's
// version, each read the one way, so the two never disagree.

// KernelReleasePath is the file the running kernel's release is read from.
const KernelReleasePath = "/proc/sys/kernel/osrelease"

// KernelRelease reads the running kernel's release, as `uname -r` prints
// it, in the caller's own process. A read that fails, or an empty file, is
// an error.
func KernelRelease(ctx context.Context, oc common.OsClient) (string, error) {
	cctx, cancel := cmdCtx(ctx)
	defer cancel()
	data, err := oc.ReadFile(cctx, KernelReleasePath)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", KernelReleasePath, err)
	}
	release := strings.TrimSpace(data)
	if release == "" {
		return "", fmt.Errorf("%s is empty", KernelReleasePath)
	}
	return release, nil
}

// kernelSeriesPattern is the series a release starts with: "7.0" of
// "7.0.0-42-generic".
var kernelSeriesPattern = regexp.MustCompile(`^([0-9]+)\.([0-9]+)`)

// KernelSeries is the major and minor number a kernel release starts with;
// ok is false for a release that does not start with them.
func KernelSeries(release string) (major, minor int, ok bool) {
	return leadingVersion(kernelSeriesPattern, release)
}

// MdadmVersion runs `mdadm --version` under the soft timeout (SH15), by the
// name the agent runs mdadm by, or by the path mdadm names, and returns the
// line it prints, which mdadm writes on stderr ("mdadm - v4.5"). A
// run that fails, or prints no line, is an error.
func MdadmVersion(
	ctx context.Context,
	oc common.OsClient,
	mdadm string,
) (string, error) {
	cmd := NewCmd(oc)
	stdout, stderr, exitCode, err := cmd.Run(ctx, mdadm, "--version")
	if err != nil {
		return "", cmdError(mdadm, []string{"--version"}, stdout, stderr,
			exitCode, err)
	}
	line := strings.TrimSpace(stderr)
	if line == "" {
		line = strings.TrimSpace(stdout)
	}
	if line == "" {
		return "", fmt.Errorf("%s --version printed nothing", mdadm)
	}
	return strings.SplitN(line, "\n", 2)[0], nil
}

// mdadmVersionPattern is the version in mdadm's version line, after "v":
// "4.5" of "mdadm - v4.5 - Ubuntu 4.5-5ubuntu1", and of a build
// from git, "mdadm - v4.6-12-gabc… - …".
var mdadmVersionPattern = regexp.MustCompile(`\bv([0-9]+)\.([0-9]+)`)

// MdadmSeries is the major and minor number of mdadm's version line; ok is
// false for a line that names none.
func MdadmSeries(line string) (major, minor int, ok bool) {
	return leadingVersion(mdadmVersionPattern, line)
}

func leadingVersion(
	pattern *regexp.Regexp,
	text string,
) (major, minor int, ok bool) {
	m := pattern.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return 0, 0, false
	}
	major, errMajor := strconv.Atoi(m[1])
	minor, errMinor := strconv.Atoi(m[2])
	if errMajor != nil || errMinor != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// LogStartVersions logs an agent's start record, "agent node versions"
// (log.md, Agent start): the kernel release, and, when withMdadm is set,
// as for the cn role, the version line of the mdadm the agent runs. A read
// that fails leaves its attribute empty and puts its error in the record,
// which is then a warning; the start goes on either way, since nothing an
// agent reads of its node stops its start, only its flags' checks do
// (dnagent.md CM3, CM4; cnagent.md CN-CM2).
func LogStartVersions(ctx context.Context, oc common.OsClient, withMdadm bool) {
	var errs []error
	release, err := KernelRelease(ctx, oc)
	if err != nil {
		errs = append(errs, err)
	}
	attrs := []any{slog.String("kernel_release", release)}
	if withMdadm {
		line, err := MdadmVersion(ctx, oc, "mdadm")
		if err != nil {
			errs = append(errs, err)
		}
		attrs = append(attrs, slog.String("mdadm_version", line))
	}
	if err := errors.Join(errs...); err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
		slog.WarnContext(ctx, "agent node versions", attrs...)
		return
	}
	slog.InfoContext(ctx, "agent node versions", attrs...)
}
