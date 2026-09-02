package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// NvmeHost wraps the nvme-cli host patterns of Appendix A. Every dnv-internal
// connection is made with --fast-io-fail-tmo DefaultNvmeFastIoFailTmo and
// --ctrl-loss-tmo -1 (SH20).
type NvmeHost struct {
	osBase
}

func NewNvmeHost(oc common.OsClient) *NvmeHost {
	return &NvmeHost{osBase{oc: oc}}
}

// TrConf is one NVMe-oF transport endpoint.
type TrConf struct {
	TrType  string
	AdrFam  string
	TrAddr  string
	TrSvcId string
}

func (h *NvmeHost) Connect(
	ctx context.Context,
	tr TrConf,
	nqn string,
	hostNqn string,
) error {
	return h.runOk(ctx, "nvme", "connect",
		"--transport", tr.TrType,
		"--traddr", tr.TrAddr,
		"--trsvcid", tr.TrSvcId,
		"--nqn", nqn,
		"--hostnqn", hostNqn,
		"--fast-io-fail-tmo",
		strconv.Itoa(common.DefaultNvmeFastIoFailTmo),
		"--ctrl-loss-tmo", "-1")
}

func (h *NvmeHost) Disconnect(ctx context.Context, nqn string) error {
	return h.runOk(ctx, "nvme", "disconnect", "--nqn", nqn)
}

// SubsysState is what `nvme list-subsys -o json` says about one subsystem.
type SubsysState struct {
	Found      bool
	Live       bool
	States     []string
	DevicePath string
}

// ---------------------------------------------------------------------------
// nvme list-subsys -o json. The top level is an array of host entries in
// nvme-cli 2.x and a bare object in older builds; both are accepted.
// ---------------------------------------------------------------------------

type nvmeNamespaceEntry struct {
	NameSpace string `json:"NameSpace"`
	Name      string `json:"Name"`
	NSID      int    `json:"NSID"`
}

type nvmePathEntry struct {
	Name       string               `json:"Name"`
	Transport  string               `json:"Transport"`
	Address    string               `json:"Address"`
	State      string               `json:"State"`
	Namespaces []nvmeNamespaceEntry `json:"Namespaces"`
}

type nvmeSubsysEntry struct {
	Name       string               `json:"Name"`
	NQN        string               `json:"NQN"`
	Paths      []nvmePathEntry      `json:"Paths"`
	Namespaces []nvmeNamespaceEntry `json:"Namespaces"`
}

type nvmeHostEntry struct {
	HostNQN    string            `json:"HostNQN"`
	Subsystems []nvmeSubsysEntry `json:"Subsystems"`
}

func parseListSubsys(stdout string) ([]nvmeSubsysEntry, error) {
	text := strings.TrimSpace(stdout)
	if text == "" {
		return nil, nil
	}
	var hosts []nvmeHostEntry
	if err := json.Unmarshal([]byte(text), &hosts); err == nil {
		var out []nvmeSubsysEntry
		for _, host := range hosts {
			out = append(out, host.Subsystems...)
		}
		return out, nil
	}
	var host nvmeHostEntry
	if err := json.Unmarshal([]byte(text), &host); err != nil {
		return nil, fmt.Errorf("nvme list-subsys json: %w", err)
	}
	return host.Subsystems, nil
}

// ListSubsys probes one subsystem NQN: whether the host holds a controller
// for it, whether any path is live, and the block device of its namespace.
func (h *NvmeHost) ListSubsys(
	ctx context.Context,
	nqn string,
) (*SubsysState, error) {
	stdout, stderr, _, err := h.run(ctx, "nvme", "list-subsys", "-o", "json")
	if err != nil {
		return nil, cmdError("nvme", []string{"list-subsys"},
			stdout, stderr, err)
	}
	subsystems, err := parseListSubsys(stdout)
	if err != nil {
		return nil, err
	}
	state := &SubsysState{}
	for _, subsys := range subsystems {
		if subsys.NQN != nqn {
			continue
		}
		state.Found = true
		namespaces := subsys.Namespaces
		for _, path := range subsys.Paths {
			state.States = append(state.States, path.State)
			if path.State == "live" {
				state.Live = true
			}
			namespaces = append(namespaces, path.Namespaces...)
		}
		for _, ns := range namespaces {
			name := ns.NameSpace
			if name == "" {
				name = ns.Name
			}
			if name != "" {
				state.DevicePath = "/dev/" + name
				break
			}
		}
		break
	}
	return state, nil
}
