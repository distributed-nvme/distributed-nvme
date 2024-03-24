package lib

import (
	"fmt"
	"strconv"
	"strings"
)

type KeyFmt struct {
	prefix string
}

func (kf *KeyFmt) ClusterEntityKey() string {
	return fmt.Sprintf("/%s/cluster", kf.prefix)
}

func (kf *KeyFmt) GlobalSummaryEntityKey() string {
	return fmt.Sprintf("/%s/global_summary", kf.prefix)
}

func (kf *KeyFmt) DnGlobalEntityKey() string {
	return fmt.Sprintf("/%s/dn_global", kf.prefix)
}

func (kf *KeyFmt) CnGlobalEntityKey() string {
	return fmt.Sprintf("/%s/cn_global", kf.prefix)
}

func (kf *KeyFmt) SpGlobalEntityKey() string {
	return fmt.Sprintf("/%s/sp_global", kf.prefix)
}

func (kf *KeyFmt) DnEntityPrefix() string {
	return fmt.Sprintf("/%s/dn/", kf.prefix)
}

func (kf *KeyFmt) DnEntityKey(dnId string) string {
	return fmt.Sprintf("%s%s", kf.DnEntityPrefix(), dnId)
}

func (kf *KeyFmt) CnEntityPrefix() string {
	return fmt.Sprintf("/%s/cn/", kf.prefix)
}

func (kf *KeyFmt) CnEntityKey(cnId string) string {
	return fmt.Sprintf("%s%s", kf.CnEntityPrefix(), cnId)
}

func (kf *KeyFmt) SpEntityPrefix() string {
	return fmt.Sprintf("/%s/sp/", kf.prefix)
}

func (kf *KeyFmt) SpEntityKey(spId string) string {
	return fmt.Sprintf("%s%s", kf.SpEntityPrefix(), spId)
}

func (kf *KeyFmt) NameToIdEntityKey(name string) string {
	return fmt.Sprintf("/%s/name_to_id/%s", kf.prefix, name)
}

func (kf *KeyFmt) TagNameEntityPrefix() string {
	return fmt.Sprintf("/%s/tag_name/", kf.prefix)
}

func (kf *KeyFmt) TagNameEntityKey(tagName string) string {
	return fmt.Sprintf("%s%s", kf.TagNameEntityPrefix(), tagName)
}

func (kf *KeyFmt) TagValueEntityPrefix(tagName string) string {
	return fmt.Sprintf("/%s/tag_value/%s/", kf.prefix, tagName)
}

func (kf *KeyFmt) TagValueEntityKey(tagName, tagValue string) string {
	return fmt.Sprintf("%s%s", kf.TagValueEntityPrefix(tagName), tagValue)
}

func (kf *KeyFmt) DnLockPath() string {
	return fmt.Sprintf("/%s/lock/dn", kf.prefix)
}

func (kf *KeyFmt) CnLockPath() string {
	return fmt.Sprintf("/%s/lock/cn", kf.prefix)
}

func (kf *KeyFmt) SpLockPath() string {
	return fmt.Sprintf("/%s/lock/sp", kf.prefix)
}

func (kf *KeyFmt) shardKeyDecode(prefix, key string) (uint8, string, error) {
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

func (kf *KeyFmt) DnShardPrefix() string {
	return fmt.Sprintf("/%s/dn_shard/", kf.prefix)
}

func (kf *KeyFmt) DnShardKeyEncode(leadingNum uint8, sockAddr string) string {
	return fmt.Sprintf("%s%d@%s", kf.DnShardPrefix(), leadingNum, sockAddr)
}

func (kf *KeyFmt) DnShardKeyDecode(key string) (uint8, string, error) {
	return kf.shardKeyDecode(kf.DnShardPrefix(), key)
}

func (kf *KeyFmt) CnShardPrefix() string {
	return fmt.Sprintf("/%s/cn_shard/", kf.prefix)
}

func (kf *KeyFmt) CnShardKeyEncode(leadingNum uint8, sockAddr string) string {
	return fmt.Sprintf("%s%d@%s", kf.CnShardPrefix(), leadingNum, sockAddr)
}

func (kf *KeyFmt) CnShardKeyDecode(key string) (uint8, string, error) {
	return kf.shardKeyDecode(kf.CnShardPrefix(), key)
}

func (kf *KeyFmt) SpShardPrefix() string {
	return fmt.Sprintf("/%s/sp_shard/", kf.prefix)
}

func (kf *KeyFmt) SpShardKeyEncode(leadingNum uint8, sockAddr string) string {
	return fmt.Sprintf("%s%d@%s", kf.SpShardPrefix(), leadingNum, sockAddr)
}

func (kf *KeyFmt) SpShardKeyDecode(key string) (uint8, string, error) {
	return kf.shardKeyDecode(kf.SpShardPrefix(), key)
}

func NewKeyFmt(prefix string) *KeyFmt {
	return &KeyFmt{
		prefix: prefix,
	}
}
