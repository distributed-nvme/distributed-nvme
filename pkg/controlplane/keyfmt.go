package controlplane

import (
	"fmt"
	"strconv"
	"strings"
)

type keyFmt struct {
	prefix string
}

func (kf *keyFmt) ClusterEntityKey() string {
	return fmt.Sprintf("/%s/cluster", kf.prefix)
}

func (kf *keyFmt) GlobalSummaryEntityKey() string {
	return fmt.Sprintf("/%s/global_summary", kf.prefix)
}

func (kf *keyFmt) DnGlobalEntityKey() string {
	return fmt.Sprintf("/%s/dn_global", kf.prefix)
}

func (kf *keyFmt) CnGlobalEntityKey() string {
	return fmt.Sprintf("/%s/cn_global", kf.prefix)
}

func (kf *keyFmt) SpGlobalEntityKey() string {
	return fmt.Sprintf("/%s/sp_global", kf.prefix)
}

func (kf *keyFmt) DnEntityPrefix() string {
	return fmt.Sprintf("/%s/dn/", kf.prefix)
}

func (kf *keyFmt) DnEntityKey(dnId string) string {
	return fmt.Sprintf("%s%s", kf.DnEntityPrefix(), dnId)
}

func (kf *keyFmt) CnEntityPrefix() string {
	return fmt.Sprintf("/%s/cn/", kf.prefix)
}

func (kf *keyFmt) CnEntityKey(cnId string) string {
	return fmt.Sprintf("%s%s", kf.CnEntityPrefix(), cnId)
}

func (kf *keyFmt) SpEntityPrefix() string {
	return fmt.Sprintf("/%s/sp/", kf.prefix)
}

func (kf *keyFmt) SpEntityKey(spId string) string {
	return fmt.Sprintf("%s%s", kf.SpEntityPrefix(), spId)
}

func (kf *keyFmt) NameToIdEntityKey(name string) string {
	return fmt.Sprintf("/%s/name_to_id/%s", kf.prefix, name)
}

func (kf *keyFmt) TagNameEntityPrefix() string {
	return fmt.Sprintf("/%s/tag_name/", kf.prefix)
}

func (kf *keyFmt) TagNameEntityKey(tagName string) string {
	return fmt.Sprintf("%s%s", kf.TagNameEntityPrefix(), tagName)
}

func (kf *keyFmt) TagValueEntityPrefix(tagName string) string {
	return fmt.Sprintf("/%s/tag_value/%s/", kf.prefix, tagName)
}

func (kf *keyFmt) TagValueEntityKey(tagName, tagValue string) string {
	return fmt.Sprintf("%s%s", kf.TagValueEntityPrefix(tagName), tagValue)
}

func (kf *keyFmt) DnLockPath() string {
	return fmt.Sprintf("/%s/lock/dn", kf.prefix)
}

func (kf *keyFmt) CnLockPath() string {
	return fmt.Sprintf("/%s/lock/cn", kf.prefix)
}

func (kf *keyFmt) SpLockPath() string {
	return fmt.Sprintf("/%s/lock/sp", kf.prefix)
}

func (kf *keyFmt) shardKeyDecode(prefix, key string) (uint8, string, error) {
	if !strings.HasPrefix(key, prefix) {
		return uint8(0), "", fmt.Errorf("Invalid key: %s", key)
	}
	items := strings.Split(key[len(prefix):], "@")
	if len(items) != 2 {
		return uint8(0), "", fmt.Errorf("Invalid key: %s", key)
	}
	leadingNum, err := strconv.ParseUint(items[0], 10, 32)
	if err != nil {
		return uint8(0), "", err
	}
	return uint8(leadingNum), items[1], nil
}

func (kf *keyFmt) DnShardPrefix() string {
	return fmt.Sprintf("/%s/dn_shard/", kf.prefix)
}

func (kf *keyFmt) DnShardKeyEncode(leadingNum uint8, sockAddr string) string {
	return fmt.Sprintf("%s%d@%s", kf.DnShardPrefix(), leadingNum, sockAddr)
}

func (kf *keyFmt) DnShardKeyDecode(key string) (uint8, string, error) {
	return kf.shardKeyDecode(kf.DnShardPrefix(), key)
}

func (kf *keyFmt) CnShardPrefix() string {
	return fmt.Sprintf("/%s/cn_shard/", kf.prefix)
}

func (kf *keyFmt) CnShardKeyEncode(leadingNum uint8, sockAddr string) string {
	return fmt.Sprintf("%s%d@%s", kf.CnShardPrefix(), leadingNum, sockAddr)
}

func (kf *keyFmt) CnShardKeyDecode(key string) (uint8, string, error) {
	return kf.shardKeyDecode(kf.CnShardPrefix(), key)
}

func (kf *keyFmt) SpShardPrefix() string {
	return fmt.Sprintf("/%s/sp_shard/", kf.prefix)
}

func (kf *keyFmt) SpShardKeyEncode(leadingNum uint8, sockAddr string) string {
	return fmt.Sprintf("%s%d@%s", kf.SpShardPrefix(), leadingNum, sockAddr)
}

func (kf *keyFmt) SpShardKeyDecode(key string) (uint8, string, error) {
	return kf.shardKeyDecode(kf.SpShardPrefix(), key)
}

func newKeyFmt(prefix string) *keyFmt {
	return &keyFmt{
		prefix: prefix,
	}
}
