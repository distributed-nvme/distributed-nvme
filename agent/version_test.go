package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
)

func TestKernelSeries(t *testing.T) {
	for release, want := range map[string][3]int{
		"7.0.0-42-generic": {7, 0, 1},
		"7.0.0-43-generic": {7, 0, 1},
		"6.19.2":           {6, 19, 1},
		"6.1.0-31-amd64":   {6, 1, 1},
		"5.15.0-1-generic": {5, 15, 1},
		" 7.1.0\n":         {7, 1, 1},
		"generic":          {0, 0, 0},
		"7":                {0, 0, 0},
		"":                 {0, 0, 0},
	} {
		major, minor, ok := KernelSeries(release)
		okInt := 0
		if ok {
			okInt = 1
		}
		if [3]int{major, minor, okInt} != want {
			t.Errorf("KernelSeries(%q) = %d, %d, %v; want %v", release,
				major, minor, ok, want)
		}
	}
}

func TestMdadmSeries(t *testing.T) {
	for line, want := range map[string][3]int{
		"mdadm - v4.5 - Ubuntu 4.5-5ubuntu1":   {4, 5, 1},
		"mdadm - v4.3 - Ubuntu 4.3-1ubuntu2.1": {4, 3, 1},
		"mdadm - v4.6-12-gabcdef0":             {4, 6, 1},
		"mdadm - v10.0":                        {10, 0, 1},
		"mdadm - unknown":                      {0, 0, 0},
		"mdadm - unknown - Ubuntu 4.5-5":       {0, 0, 0},
		"":                                     {0, 0, 0},
	} {
		major, minor, ok := MdadmSeries(line)
		okInt := 0
		if ok {
			okInt = 1
		}
		if [3]int{major, minor, okInt} != want {
			t.Errorf("MdadmSeries(%q) = %d, %d, %v; want %v", line, major,
				minor, ok, want)
		}
	}
}

// mdadm writes its version line on stderr; the reader takes it from there.
func TestMdadmVersionReadsStderr(t *testing.T) {
	oc := &common.FakeOsClient{RunCommandFn: func(_ context.Context,
		name string, args []string, _ string) (string, string, int, error) {
		if name != "/sbin/mdadm" || strings.Join(args, " ") != "--version" {
			t.Errorf("ran %s %v", name, args)
		}
		return "", "mdadm - v4.5\n", 0, nil
	}}
	line, err := MdadmVersion(context.Background(), oc, "/sbin/mdadm")
	if err != nil || line != "mdadm - v4.5" {
		t.Errorf("MdadmVersion = %q, %v", line, err)
	}
}

// captureLog swaps the default logger for one that writes JSON records into
// the returned buffer, until the test ends.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf,
		&slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

// startRecord is the one "agent node versions" record in the log.
func startRecord(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var found map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()),
		"\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["msg"] == "agent node versions" {
			if found != nil {
				t.Errorf("two start records")
			}
			found = rec
		}
	}
	if found == nil {
		t.Fatalf("no start record in:\n%s", buf.String())
	}
	return found
}

// The cn agent's start record carries the build version, the kernel release
// and mdadm's version line; the dn agent's the build version and the kernel
// release.
func TestLogStartVersions(t *testing.T) {
	oc := &common.FakeOsClient{
		ReadFileFn: func(_ context.Context, path string) (string, error) {
			if path != KernelReleasePath {
				t.Errorf("read %s", path)
			}
			return "7.0.0-42-generic\n", nil
		},
		RunCommandFn: func(_ context.Context, name string, args []string,
			_ string) (string, string, int, error) {
			return "", "mdadm - v4.5\n", 0, nil
		},
	}
	buf := captureLog(t)
	LogStartVersions(context.Background(), oc, true)
	rec := startRecord(t, buf)
	if rec["level"] != "INFO" ||
		rec["version"] != common.BuildVersion() ||
		rec["kernel_release"] != "7.0.0-42-generic" ||
		rec["mdadm_version"] != "mdadm - v4.5" ||
		rec["error"] != nil {
		t.Errorf("cn start record = %v", rec)
	}
	buf = captureLog(t)
	LogStartVersions(context.Background(), oc, false)
	rec = startRecord(t, buf)
	if _, ok := rec["mdadm_version"]; ok || rec["version"] != common.BuildVersion() {
		t.Errorf("dn start record names mdadm or lacks the build version: %v", rec)
	}
}

// A read that fails is logged in the record, which is then a warning, and
// LogStartVersions returns: the start goes on.
func TestLogStartVersionsGoesOnAfterAFailedRead(t *testing.T) {
	oc := &common.FakeOsClient{
		ReadFileFn: func(_ context.Context, path string) (string, error) {
			return "", &fs.PathError{Op: "open", Path: path,
				Err: fs.ErrPermission}
		},
		RunCommandFn: func(_ context.Context, name string, args []string,
			_ string) (string, string, int, error) {
			return "", "mdadm: not found", 127, errors.New("exit status 127")
		},
	}
	buf := captureLog(t)
	LogStartVersions(context.Background(), oc, true)
	rec := startRecord(t, buf)
	errText, _ := rec["error"].(string)
	if rec["level"] != "WARN" || !strings.Contains(errText, KernelReleasePath) ||
		!strings.Contains(errText, "mdadm") || rec["kernel_release"] != "" {
		t.Errorf("start record = %v", rec)
	}
}
