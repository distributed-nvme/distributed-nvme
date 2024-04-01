package controlplane

import (
	"fmt"
	"strings"
)

type keyFmt struct {
	prefix string
}

func (kf *keyFmt) clusterEntityKey() string {
	return fmt.Sprintf("/%s/cluster", kf.prefix)
}

func (kf *keyFmt) globalSummaryEntityKey() string {
	return fmt.Sprintf("/%s/global_summary", kf.prefix)
}

func (kf *keyFmt) dnGlobalEntityKey() string {
	return fmt.Sprintf("/%s/dn_global", kf.prefix)
}

func (kf *keyFmt) cnGlobalEntityKey() string {
	return fmt.Sprintf("/%s/cn_global", kf.prefix)
}

func (kf *keyFmt) spGlobalEntityKey() string {
	return fmt.Sprintf("/%s/sp_global", kf.prefix)
}

func (kf *keyFmt) dnEntityPrefix() string {
	return fmt.Sprintf("/%s/dn/", kf.prefix)
}

func (kf *keyFmt) dnEntityKey(dnId string) string {
	return fmt.Sprintf("%s%s", kf.dnEntityPrefix(), dnId)
}

func (kf *keyFmt) cnEntityPrefix() string {
	return fmt.Sprintf("/%s/cn/", kf.prefix)
}

func (kf *keyFmt) cnEntityKey(cnId string) string {
	return fmt.Sprintf("%s%s", kf.cnEntityPrefix(), cnId)
}

func (kf *keyFmt) spEntityPrefix() string {
	return fmt.Sprintf("/%s/sp/", kf.prefix)
}

func (kf *keyFmt) spEntityKey(spId string) string {
	return fmt.Sprintf("%s%s", kf.spEntityPrefix(), spId)
}

func (kf *keyFmt) nameToIdEntityKey(name string) string {
	return fmt.Sprintf("/%s/name_to_id/%s", kf.prefix, name)
}

func (kf *keyFmt) tagNameEntityPrefix() string {
	return fmt.Sprintf("/%s/tag_name/", kf.prefix)
}

func (kf *keyFmt) tagNameEntityKey(tagName string) string {
	return fmt.Sprintf("%s%s", kf.tagNameEntityPrefix(), tagName)
}

func (kf *keyFmt) tagValueEntityPrefix(tagName string) string {
	return fmt.Sprintf("/%s/tag_value/%s/", kf.prefix, tagName)
}

func (kf *keyFmt) tagValueEntityKey(tagName, tagValue string) string {
	return fmt.Sprintf("%s%s", kf.tagValueEntityPrefix(tagName), tagValue)
}

func (kf *keyFmt) dnLockPath() string {
	return fmt.Sprintf("/%s/lock/dn", kf.prefix)
}

func (kf *keyFmt) cnLockPath() string {
	return fmt.Sprintf("/%s/lock/cn", kf.prefix)
}

func (kf *keyFmt) spLockPath() string {
	return fmt.Sprintf("/%s/lock/sp", kf.prefix)
}

func shardKeyDecode(prefix, key string) (string, string, error) {
	if !strings.HasPrefix(key, prefix) {
		return "", "", fmt.Errorf("Invalid key: %s", key)
	}
	items := strings.Split(key[len(prefix):], "@")
	if len(items) != 2 {
		return "", "", fmt.Errorf("Invalid key: %s", key)
	}
	return items[0], items[1], nil
}

func (kf *keyFmt) dnShardPrefix() string {
	return fmt.Sprintf("/%s/dn_shard/", kf.prefix)
}

func (kf *keyFmt) dnShardKeyEncode(prioCode string, grpcTarget string) string {
	return fmt.Sprintf("%s%s@%s", kf.dnShardPrefix(), prioCode, grpcTarget)
}

func (kf *keyFmt) dnShardKeyDecode(key string) (string, string, error) {
	return shardKeyDecode(kf.dnShardPrefix(), key)
}

func (kf *keyFmt) cnShardPrefix() string {
	return fmt.Sprintf("/%s/cn_shard/", kf.prefix)
}

func (kf *keyFmt) cnShardKeyEncode(prioCode string, grpcTarget string) string {
	return fmt.Sprintf("%s%s@%s", kf.cnShardPrefix(), prioCode, grpcTarget)
}

func (kf *keyFmt) cnShardKeyDecode(key string) (string, string, error) {
	return shardKeyDecode(kf.cnShardPrefix(), key)
}

func (kf *keyFmt) spShardPrefix() string {
	return fmt.Sprintf("/%s/sp_shard/", kf.prefix)
}

func (kf *keyFmt) spShardKeyEncode(prioCode string, grpcTarget string) string {
	return fmt.Sprintf("%s%s@%s", kf.spShardPrefix(), prioCode, grpcTarget)
}

func (kf *keyFmt) spShardKeyDecode(key string) (string, string, error) {
	return shardKeyDecode(kf.spShardPrefix(), key)
}

func newKeyFmt(prefix string) *keyFmt {
	return &keyFmt{
		prefix: prefix,
	}
}
