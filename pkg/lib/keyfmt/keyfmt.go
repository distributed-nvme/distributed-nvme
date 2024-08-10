package keyfmt

import (
	"fmt"
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

func (kf *KeyFmt) DnConfEntityPrefix() string {
	return fmt.Sprintf("/%s/dn_conf", kf.prefix)
}

func (kf *KeyFmt) DnConfEntityKey(dnId string) string {
	return fmt.Sprintf("%s/%s", kf.DnConfEntityPrefix(), dnId)
}

func (kf *KeyFmt) DnInfoEntityPrefix() string {
	return fmt.Sprintf("/%s/dn_info", kf.prefix)
}

func (kf *KeyFmt) DnInfoEntityKey(dnId string) string {
	return fmt.Sprintf("%s/%s", kf.DnInfoEntityPrefix(), dnId)
}

func (kf *KeyFmt) CnConfEntityPrefix() string {
	return fmt.Sprintf("/%s/cn_conf", kf.prefix)
}

func (kf *KeyFmt) CnConfEntityKey(cnId string) string {
	return fmt.Sprintf("%s/%s", kf.CnConfEntityPrefix(), cnId)
}

func (kf *KeyFmt) CnInfoEntityPrefix() string {
	return fmt.Sprintf("/%s/cn_info", kf.prefix)
}

func (kf *KeyFmt) CnInfoEntityKey(cnId string) string {
	return fmt.Sprintf("%s/%s", kf.CnInfoEntityPrefix(), cnId)
}

func (kf *KeyFmt) SpConfEntityPrefix() string {
	return fmt.Sprintf("/%s/sp_conf", kf.prefix)
}

func (kf *KeyFmt) SpConfEntityKey(spId string) string {
	return fmt.Sprintf("%s/%s", kf.SpConfEntityPrefix(), spId)
}

func (kf *KeyFmt) SpInfoEntityPrefix() string {
	return fmt.Sprintf("/%s/sp_info", kf.prefix)
}

func (kf *KeyFmt) SpInfoEntityKey(spId string) string {
	return fmt.Sprintf("%s/%s", kf.SpInfoEntityPrefix(), spId)
}

func (kf *KeyFmt) NameToIdEntityKey(name string) string {
	return fmt.Sprintf("/%s/name_to_id/%s", kf.prefix, name)
}

func (kf *KeyFmt) TagNameEntityPrefix() string {
	return fmt.Sprintf("/%s/tag_name", kf.prefix)
}

func (kf *KeyFmt) TagNameEntityKey(tagName string) string {
	return fmt.Sprintf("%s/%s", kf.TagNameEntityPrefix(), tagName)
}

func (kf *KeyFmt) TagValueEntityPrefix(tagName string) string {
	return fmt.Sprintf("/%s/tag_value/%s", kf.prefix, tagName)
}

func (kf *KeyFmt) TagValueEntityKey(tagName, tagValue string) string {
	return fmt.Sprintf("%s/%s", kf.TagValueEntityPrefix(tagName), tagValue)
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

func (kf *KeyFmt) DnMemberPrefix() string {
	return fmt.Sprintf("/%s/dn_member/", kf.prefix)
}

func (kf *KeyFmt) CnMemberPrefix() string {
	return fmt.Sprintf("/%s/cn_member/", kf.prefix)
}

func (kf *KeyFmt) SpMemberPrefix() string {
	return fmt.Sprintf("/%s/sp_member/", kf.prefix)
}

func (kf *KeyFmt) AllocLockPath() string {
	return fmt.Sprintf("/%s/alloc_lock", kf.prefix)
}

func NewKeyFmt(prefix string) *KeyFmt {
	return &KeyFmt{
		prefix: prefix,
	}
}
