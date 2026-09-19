package agent

import (
	"context"
	"errors"
	"os/exec"
	"sort"
	"strconv"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// ---------------------------------------------------------------------------
// "Did not answer" is not "absent"
// ---------------------------------------------------------------------------

// osAnswer is which of the two failure outcomes the OS gives the primitive
// under test. They are the two halves the whole teardown-by-sweep design
// rests on, and conflating them is the defect it was written to fix: a
// removal that was skipped because a killed probe said "already gone", an
// object forgotten together with the plan that named it, and nothing left on
// the node that would ever enumerate it again.
type osAnswer int

const (
	// answeredNo: the tool ran, looked, and reported that the object is not
	// there — a non-zero exit, or an ENOENT from a virtual-filesystem read.
	answeredNo osAnswer = iota
	// didNotAnswer: the tool never reported. Killed at the SH15 soft or hard
	// timeout, failed to start, ctx cancelled, semaphore refused — exit code
	// -1 with a non-nil error. The caller learned NOTHING, and in particular
	// did not learn that the object is gone: a killed command may still have
	// completed in the kernel, because the ioctl finishes regardless of the
	// signal.
	didNotAnswer
)

// TestKilledProbeIsNotAbsent pins the rule every enumerator and every removal
// verification in a sweep depends on: a probe that did not answer returns an
// ERROR, and only a probe that ran and answered "no" returns absence.
//
// The rule has to hold per primitive, not just in the helper, because each of
// these is the last place an OS outcome is a pair of values and the first
// place it is a verdict. Below them sits a caller whose next move on "absent"
// is to skip a removal, free an extent record, or stop tracking the object —
// all of which are irreversible on a node where the object is in fact still
// there.
//
// The second half of each sub-test matters as much as the first: a primitive
// that turned every non-zero exit into an error would report a leftover on
// every pass over a node that is already clean, and a sweep that can never
// reach "clean" never lets its worker's retry loop stop.
func TestKilledProbeIsNotAbsent(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		// probe drives one primitive against an OS whose single relevant
		// operation gives `answer`, and reports whether the primitive
		// concluded "the object is not there".
		probe func(answer osAnswer) (absent bool, err error)
	}{
		{
			// Dm.Info is the verification of every dm removal on both roles.
			// On the DN its "gone" also gates meta.FreeSide: a killed
			// `dmsetup info` read as absence frees the extent record of a
			// device that still maps those extents, and the next allocation
			// hands them to a second side. That is a corruption path, which
			// is why this one is not merely a leak.
			name: "Dm.Info",
			probe: func(answer osAnswer) (bool, error) {
				info, err := NewDm(cmdAnswering(answer)).Info(ctx, "dnv-leg")
				return info == nil, err
			},
		},
		{
			// osBase.listDir, through the enumerator that walks configfs.
			// RemoveSubsystem and RemovePortLink walk their children through
			// the same listing, so a killed `ls` used to make them skip every
			// object they should have removed, silently and with no error.
			name: "listDir",
			probe: func(answer osAnswer) (bool, error) {
				fs := &fakeSysfs{
					dirs:   map[string][]string{},
					files:  map[string]string{},
					killLs: map[string]bool{},
				}
				if answer == didNotAnswer {
					fs.killLs[NvmetRoot+"/subsystems"] = true
				}
				// answeredNo needs no fixture: `ls` of a directory this tree
				// does not hold exits non-zero, exactly as it does on a node
				// whose nvmet module was never loaded.
				got, err := NewNvmet(fs.osClient()).ListSubsystems(ctx)
				return len(got) == 0, err
			},
		},
		{
			// NvmeHost.readTrimmed, through the walk that answers "does this
			// host hold a connection to this subsystem". Its absence answer
			// disconnects nothing and forgets the subsystem; the /sys/class/
			// nvme tree stalls precisely while a controller is mid-reset or
			// being torn down, which is when this walk runs.
			name: "NvmeHost.readTrimmed",
			probe: func(answer osAnswer) (bool, error) {
				const nqn = "nqn.2024-01.io.dnv:2:a:b:c:d"
				const subsysNqnPath = sysfsNvmeSubsysDir +
					"/nvme-subsys0/subsysnqn"
				fs := &fakeSysfs{
					dirs: map[string][]string{
						sysfsNvmeSubsysDir:                   {"nvme-subsys0"},
						sysfsNvmeSubsysDir + "/nvme-subsys0": {"subsysnqn"},
					},
					// answeredNo: the attribute is not there at all, the
					// ENOENT a subsystem that went away between the listing
					// and the read gives.
					files:   map[string]string{},
					readErr: map[string]error{},
				}
				if answer == didNotAnswer {
					fs.readErr[subsysNqnPath] = context.DeadlineExceeded
				}
				state, err := NewNvmeHost(fs.osClient()).ListSubsys(ctx, nqn)
				return state != nil && !state.Found, err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			absent, err := tc.probe(answeredNo)
			if err != nil {
				t.Fatalf("a reported \"not there\" came back as an error: %v",
					err)
			}
			if !absent {
				t.Fatal("a reported \"not there\" did not read as absent")
			}
			if _, err = tc.probe(didNotAnswer); err == nil {
				t.Fatal("a probe that never answered read as absent, " +
					"which is how a live object gets skipped and forgotten")
			}
		})
	}

	// agent.Reported itself. Every primitive above is one `if` away from the
	// bug, and this is that `if`.
	t.Run("Reported", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			exitCode int
			err      error
			want     bool
		}{
			{"the tool succeeded", 0, nil, true},
			{"the tool answered no", 1, errors.New("exit status 1"), true},
			{"the tool answered no, loudly", 2,
				errors.New("exit status 2"), true},
			{"killed at the soft timeout", -1,
				errors.New("signal: killed"), false},
			{"failed to start", -1,
				errors.New("exec: \"dmsetup\": executable file not found"),
				false},
			{"ctx cancelled", -1, context.Canceled, false},
			{"the semaphore refused it", -1, context.DeadlineExceeded, false},
		} {
			if got := Reported(tc.exitCode, tc.err); got != tc.want {
				t.Errorf("%s: Reported(%d, %v) = %v, want %v",
					tc.name, tc.exitCode, tc.err, got, tc.want)
			}
		}

		// The subtle case, and the reason Reported tests the exit code and
		// not the error's type. A process killed by a signal comes back from
		// os/exec as an *exec.ExitError — the SAME type a plain `exit 1`
		// produces — and the two are told apart only by ExitCode(), which is
		// -1 for the signalled one. A rule of the shape "it answered if the
		// error is an ExitError" would therefore call the SIGTERM of the
		// SH15 soft timeout an answer, which is exactly the conflation this
		// function exists to prevent. Both runs go through the production
		// OsClient, so what is pinned here is the real (exitCode, err) pair
		// an agent sees and not a fake's imitation of one.
		if _, err := exec.LookPath("sh"); err != nil {
			t.Skipf("no sh to signal: %v", err)
		}
		oc := common.NewLimitedOsClient(0)
		_, _, killedCode, killedErr := oc.RunCommand(
			context.Background(), "sh", []string{"-c", "kill -TERM $$"}, "")
		var exitErr *exec.ExitError
		if !errors.As(killedErr, &exitErr) {
			t.Fatalf("a signalled child gave %T (%v); the trap this case "+
				"guards is that it gives an *exec.ExitError",
				killedErr, killedErr)
		}
		if killedCode != -1 || exitErr.ExitCode() != -1 {
			t.Fatalf("a signalled child reported exit code %d/%d, want -1",
				killedCode, exitErr.ExitCode())
		}
		if Reported(killedCode, killedErr) {
			t.Fatal("a signalled child was read as an answer")
		}
		_, _, refusedCode, refusedErr := oc.RunCommand(
			context.Background(), "sh", []string{"-c", "exit 1"}, "")
		if !errors.As(refusedErr, &exitErr) {
			t.Fatalf("a refusing child gave %T (%v), want an *exec.ExitError",
				refusedErr, refusedErr)
		}
		if refusedCode != 1 {
			t.Fatalf("a refusing child reported exit code %d, want 1",
				refusedCode)
		}
		if !Reported(refusedCode, refusedErr) {
			t.Fatal("a tool that ran and answered \"no\" was read as a kill")
		}
	})
}

// cmdAnswering is an OsClient whose every command gives one of the two
// failure outcomes, in the exact shape common.OsClient.RunCommand documents:
// a reported refusal carries the tool's exit status, a kill carries -1.
func cmdAnswering(answer osAnswer) *common.FakeOsClient {
	return &common.FakeOsClient{
		RunCommandFn: func(
			_ context.Context, _ string, _ []string, _ string,
		) (string, string, int, error) {
			if answer == didNotAnswer {
				return "", "signal: killed", -1, errors.New("signal: killed")
			}
			return "", "Device does not exist.", 1,
				errors.New("exit status 1")
		},
	}
}

// ---------------------------------------------------------------------------
// Dm.List — the root of every sweep
// ---------------------------------------------------------------------------

// TestDmListParsesBothDevNoSpellings pins what `dmsetup ls` output means to a
// sweep: every line it names is a device that exists, whatever the rest of
// the line looks like, and the only thing that may ever make the answer
// "this node holds no dm devices" is a run that said so.
//
// Three shapes of line have to survive. The two device-number spellings the
// tool has used across versions — "(253:4)" and the older "(253, 4)" — must
// normalize to the same thing, because the sweep builds a devno-to-name index
// out of this column and resolves live table arguments through it; a dm table
// always prints its devices as "major:minor", so a comma-spelled listing
// would resolve none of them. An entry whose device number is missing or
// unparsable must be KEPT: it is a device the listing named and the kernel has
// already dropped, i.e. precisely an object some concurrent teardown is in the
// middle of, and dropping it would hide from the sweep the one class of object
// it exists to find — the name alone is what attributes and removes it. And
// the literal "No devices found", which an empty node prints with exit status
// 0, is not a device name.
func TestDmListParsesBothDevNoSpellings(t *testing.T) {
	ctx := context.Background()
	const listing = "dnv-leg\t(253:4)\n" +
		"dnv-pool\t(253, 5)\n" +
		"dnv-vanished\n" +
		"dnv-unparsable\t(no-such-devno)\n"
	got, err := NewDm(dmLsAnswering(listing, 0, nil)).List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, want := range []struct {
		name  string
		devNo string
	}{
		{"dnv-leg", "253:4"},
		{"dnv-pool", "253:5"},
		{"dnv-vanished", ""},
		{"dnv-unparsable", ""},
	} {
		devNo, ok := got[want.name]
		if !ok {
			t.Fatalf("%s is missing from the listing: %v", want.name, got)
		}
		if devNo != want.devNo {
			t.Errorf("%s = %q, want %q", want.name, devNo, want.devNo)
		}
	}
	if len(got) != 4 {
		t.Errorf("listing = %v, want exactly the four devices", got)
	}

	// An empty node: one literal line, exit status 0, and no devices.
	empty, err := NewDm(
		dmLsAnswering("No devices found\n", 0, nil)).List(ctx)
	if err != nil {
		t.Fatalf("List on an empty node: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("an empty node listed %v", empty)
	}

	// Neither failure may ever read as "no devices": the caller's next act on
	// an empty listing is to conclude that every dm object of every removed
	// sp is already gone.
	for _, tc := range []struct {
		name     string
		stdout   string
		exitCode int
		err      error
	}{
		{"a refused listing", "", 1, errors.New("exit status 1")},
		{"a killed listing", "", -1, errors.New("signal: killed")},
	} {
		devices, err := NewDm(
			dmLsAnswering(tc.stdout, tc.exitCode, tc.err)).List(ctx)
		if err == nil {
			t.Errorf("%s reported %v devices and no error",
				tc.name, len(devices))
		}
	}
}

// dmLsAnswering is an OsClient that answers `dmsetup ls` with one scripted
// result.
func dmLsAnswering(
	stdout string,
	exitCode int,
	err error,
) *common.FakeOsClient {
	return &common.FakeOsClient{
		RunCommandFn: func(
			_ context.Context, name string, args []string, _ string,
		) (string, string, int, error) {
			if name != "dmsetup" || len(args) != 1 || args[0] != "ls" {
				return "", "", 127, errors.New("unexpected command")
			}
			return stdout, "", exitCode, err
		},
	}
}

// ---------------------------------------------------------------------------
// nvmet enumeration
// ---------------------------------------------------------------------------

// TestNvmetListSubsystemsSeesUnlinked pins that the sweep's nvmet enumerator
// is the configfs subsystems directory and not the port's link set. The two
// differ exactly where it matters: RemoveSubsystem tears a subsystem down in
// reverse build order, so a removal that was interrupted — or killed — after
// the port link went and before the rmdir leaves a subsystem that is present,
// unlinked, and still holding its namespaces open against the devices below
// it. Enumerating the port's links would report that node clean for ever.
func TestNvmetListSubsystemsSeesUnlinked(t *testing.T) {
	ctx := context.Background()
	const linked = "nqn.2024-01.io.dnv:4:1:2:3"
	const unlinked = "nqn.2024-01.io.dnv:4:1:2:4"
	fs := &fakeSysfs{
		dirs: map[string][]string{
			NvmetRoot + "/subsystems": {linked, unlinked},
			NvmetRoot + "/ports/" +
				strconv.Itoa(common.NvmetPortId) + "/subsystems": {linked},
		},
		files: map[string]string{},
	}
	nvmet := NewNvmet(fs.osClient())

	got, err := nvmet.ListSubsystems(ctx)
	if err != nil {
		t.Fatalf("ListSubsystems: %v", err)
	}
	sort.Strings(got)
	if len(got) != 2 || got[0] != linked || got[1] != unlinked {
		t.Fatalf("subsystems = %v, want both %s and %s",
			got, linked, unlinked)
	}

	// The port's own view is the other half of the attribution, and it is
	// deliberately narrower: it is what says the link still has to go.
	onPort, err := nvmet.ListPortSubsystems(ctx, common.NvmetPortId)
	if err != nil {
		t.Fatalf("ListPortSubsystems: %v", err)
	}
	if len(onPort) != 1 || onPort[0] != linked {
		t.Fatalf("port subsystems = %v, want only %s", onPort, linked)
	}
}

// TestNsDevicePathIsStrict pins the read a host-facing subsystem's ownership
// is decided by. Such a subsystem carries a user-chosen NQN with none of our
// ids in it, so the only thing that attributes it to an sp is the device its
// namespaces point at — and the sweep's next move on a subsystem it cannot
// attribute is to remove it, taking the host's access with it. An absent
// device_path is therefore an answer ("this namespace has no backing device")
// and an unreadable one must be an error.
func TestNsDevicePathIsStrict(t *testing.T) {
	ctx := context.Background()
	const nqn = "nqn.2024-01.io.example:host-facing"
	fs := &fakeSysfs{
		dirs:    map[string][]string{},
		files:   map[string]string{},
		readErr: map[string]error{},
	}
	nvmet := NewNvmet(fs.osClient())
	// The fixture is filled through the same NsPath the production code
	// builds, so a change to the configfs layout cannot leave this test
	// passing against paths nobody reads.
	fs.files[nvmet.NsPath(nqn, 1)+"/device_path"] = "/dev/mapper/dnv-ns\n"
	// nsid 2 stalled and nsid 3 could not be read at all: the two ways a
	// read fails without the attribute being absent.
	fs.readErr[nvmet.NsPath(nqn, 2)+"/device_path"] = context.DeadlineExceeded
	fs.readErr[nvmet.NsPath(nqn, 3)+"/device_path"] = errors.New(
		"input/output error")

	path, ok, err := nvmet.NsDevicePath(ctx, nqn, 1)
	if err != nil || !ok {
		t.Fatalf("a readable device_path gave (%q, %v, %v)", path, ok, err)
	}
	if path != "/dev/mapper/dnv-ns" {
		t.Errorf("device_path = %q, want the trimmed value", path)
	}

	// Genuinely absent: an answer, and not an error. The namespace is simply
	// not there — removed between the listing that named it and this read —
	// and the attribution moves on to the next one. Erroring here would turn
	// an ordinary race into an enumeration failure, and an enumeration
	// failure is itself a leftover.
	if _, ok, err = nvmet.NsDevicePath(ctx, nqn, 4); err != nil || ok {
		t.Fatalf("an absent device_path gave (%v, %v), want (false, nil)",
			ok, err)
	}

	for _, nsid := range []int{2, 3} {
		if _, ok, err = nvmet.NsDevicePath(ctx, nqn, nsid); err == nil {
			t.Fatalf("nsid %d: a read that did not answer gave ok=%v and no "+
				"error, which would make a live subsystem look unowned",
				nsid, ok)
		}
	}
}
