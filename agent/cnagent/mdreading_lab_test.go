package cnagent

// Superblock areas md wrote on members in the lab (cn2, kernel 7.0.0-42,
// mdadm 4.5), read at 4096 bytes into each member, the zero bytes after the
// roles left out: mdSbArea pads them back. Each is the array of one run, with
// dnv's create options; the comment of each says what mdadm --examine printed
// of it.

// mdSbLabFull is a full member: Events 2, Device Role Active device 0, Feature Map 0x1.
const mdSbLabFull = "fc4e2ba9010000000100000000000000e9b826b4df21423108620ea66f8f15ac" +
	"616e793a73626578700000000000000000000000000000000000000000000000" +
	"9657fa6a00000000010000000000000000f00f00000000000000000002000000" +
	"0800000000000000000000000000000000000000000000000000000000000000" +
	"001000000000000000f00f000000000008000000000000000000000000000000" +
	"0000000000000000fcdc6f70ac703c64d8ae0a745b43a8970200080010000000" +
	"9757fa6a000000000200000000000000ffffffffffffffff36ce134a80000000" +
	"0002000000000000000000000000000000000000000000000000000000000000" +
	"00000100ffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"

// mdSbLabHalfcopied is a half-copied member: Events 11, Device Role Active device 1,
// Recovery Offset 146944 sectors, Feature Map 0x3.
const mdSbLabHalfcopied = "fc4e2ba9010000000300000000000000e9b826b4df21423108620ea66f8f15ac" +
	"616e793a73626578700000000000000000000000000000000000000000000000" +
	"9657fa6a00000000010000000000000000f00f00000000000000000002000000" +
	"0800000000000000000000000000000000000000000000000000000000000000" +
	"001000000000000000f00f00000000000800000000000000003e020000000000" +
	"02000000000000005b12180ad46b61715e406efb77ce09a50200080010000000" +
	"b557fa6a000000000b00000000000000ffffffffffffffff8d59a58580000000" +
	"0002000000000000000000000000000000000000000000000000000000000000" +
	"0000ffff0100ffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"

// mdSbLabSpare is a spare: Events 3, Device Role spare, Feature Map 0x1.
const mdSbLabSpare = "fc4e2ba9010000000100000000000000e9b826b4df21423108620ea66f8f15ac" +
	"616e793a73626578700000000000000000000000000000000000000000000000" +
	"9657fa6a00000000010000000000000000f00f00000000000000000002000000" +
	"0800000000000000000000000000000000000000000000000000000000000000" +
	"001000000000000000f00f000000000008000000000000000000000000000000" +
	"02000000000000005b12180ad46b61715e406efb77ce09a50200080010000000" +
	"9d57fa6a000000000300000000000000ffffffffffffffff681ba68580000000" +
	"0002000000000000000000000000000000000000000000000000000000000000" +
	"00000100ffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"

// mdSbLabBadblocks is a member with one bad block, "4096 for 8 sectors": Events 2, Device
// Role Active device 1, Feature Map 0x9, the list at offset 16 sectors.
const mdSbLabBadblocks = "fc4e2ba9010000000900000000000000e9b826b4df21423108620ea66f8f15ac" +
	"616e793a73626578700000000000000000000000000000000000000000000000" +
	"9657fa6a00000000010000000000000000f00f00000000000000000002000000" +
	"0800000000000000000000000000000000000000000000000000000000000000" +
	"001000000000000000f00f000000000008000000000000000000000000000000" +
	"0100000000000000f303aadc544ce599a02ab2c8bed341c80200080010000000" +
	"9857fa6a000000000200000000000000ffffffffffffffff0bdd377180000000" +
	"0002000000000000000000000000000000000000000000000000000000000000" +
	"00000100ffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
