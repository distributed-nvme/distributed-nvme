package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// Local-store file-name kind prefixes (architecture.md §4.6). The dn role
// owns the first three, the cn role the last three (SH6).
const (
	StoreKindDn      = "dn-"
	StoreKindSide    = "side-"
	StoreKindMigrBm  = "migr-bm-"
	StoreKindCn      = "cn-"
	StoreKindCntlr   = "cntlr-"
	StoreKindCloneBm = "clone-bm-"
)

// Store is the agent's local store (SH4-SH7): the last fully applied
// Syncup*Request per object plus one file per received Push*Bitmap chunk,
// all under the --local-store prefix. Writes go through OsClient.WriteProto
// (atomic replace); reads through ReadProto.
type Store struct {
	oc     common.OsClient
	prefix string
}

func NewStore(oc common.OsClient, localStorPrefix string) *Store {
	if localStorPrefix == "" {
		localStorPrefix = common.DefaultLocalStorPrefix
	}
	return &Store{oc: oc, prefix: localStorPrefix}
}

// Prefix is the directory the store lives in.
func (s *Store) Prefix() string {
	return s.prefix
}

// List enumerates the store and returns, per requested kind prefix, the full
// paths of the matching files in ascending name order (SH6). A failure to
// read the prefix is the one fatal startup condition of SH3.
func (s *Store) List(
	ctx context.Context,
	kinds ...string,
) (map[string][]string, error) {
	stdout, stderr, _, err := s.oc.RunCommand(
		ctx, "ls", []string{"-1", s.prefix}, "")
	if err != nil {
		return nil, fmt.Errorf(
			"local store %s unreadable: %w: %s", s.prefix, err, stderr)
	}
	out := make(map[string][]string, len(kinds))
	for _, kind := range kinds {
		out[kind] = nil
	}
	for _, line := range strings.Split(stdout, "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		for _, kind := range kinds {
			if strings.HasPrefix(name, kind) {
				out[kind] = append(out[kind], s.prefix+"/"+name)
				break
			}
		}
	}
	for _, kind := range kinds {
		sort.Strings(out[kind])
	}
	return out, nil
}

// Load decodes one store file into msg.
func (s *Store) Load(
	ctx context.Context,
	path string,
	msg proto.Message,
) error {
	return s.oc.ReadProto(ctx, path, msg)
}

// Save persists one store file. Callers save a Syncup*Request only after its
// converge pass completed (SH5).
func (s *Store) Save(
	ctx context.Context,
	path string,
	msg proto.Message,
) error {
	return s.oc.WriteProto(ctx, path, msg)
}

// Remove deletes store files after the resources they describe are gone
// (SH7); a crash in between simply re-runs the teardown on restart.
func (s *Store) Remove(ctx context.Context, paths ...string) error {
	if len(paths) == 0 {
		return nil
	}
	args := append([]string{"-f"}, paths...)
	_, stderr, _, err := s.oc.RunCommand(ctx, "rm", args, "")
	if err != nil {
		return fmt.Errorf("rm %v: %w: %s", paths, err, stderr)
	}
	return nil
}
