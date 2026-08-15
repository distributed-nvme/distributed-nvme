
```

package main

import (
	"fmt"
	"hash/fnv"
)

func clusterNameToId(clusterName string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(clusterName))
	return h.Sum64()
}

func getUniqId(clusterId, nodeId uint64) uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%016x%016x", clusterId, nodeId)
	return h.Sum64()
}

func showId(clusterName string, nodeId uint64) {
	clusterId := clusterNameToId(clusterName)
	uniqId := getUniqId(clusterId, nodeId)
	fmt.Printf("%20s %016x %016x %016x\n", clusterName, nodeId, clusterId, uniqId)
}

func main() {
	showId("default", 12)
	showId("_default", 12)
}
```

```
package main

import (
	"fmt"
)

// scrambleMix64 avalanches a uint64 so small inputs look random
func scrambleMix64(x uint64) uint64 {
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// UUIDFromChainedSeeds creates a unique 128-bit string layout using seed1 and seed2
func UUIDFromUniqId(uniqId uint64) string {
	const hex = "0123456789abcdef"

	// 1. Generate two independent, pseudo-random uint64 blocks
	seed1 := scrambleMix64(uniqId)
	seed2 := scrambleMix64(seed1)

	// 2. Extract 8 bytes from seed1 (First 64 bits)
	b0 := byte(seed1 >> 56)
	b1 := byte(seed1 >> 48)
	b2 := byte(seed1 >> 40)
	b3 := byte(seed1 >> 32)
	b4 := byte(seed1 >> 24)
	b5 := byte(seed1 >> 16)
	b6 := byte(seed1 >> 8)
	b7 := byte(seed1)

	// 3. Extract 8 bytes from seed2 (Second 64 bits)
	b8 := byte(seed2 >> 56)
	b9 := byte(seed2 >> 48)
	b10 := byte(seed2 >> 40)
	b11 := byte(seed2 >> 32)
	b12 := byte(seed2 >> 24)
	b13 := byte(seed2 >> 16)
	b14 := byte(seed2 >> 8)
	b15 := byte(seed2)

	// 4. Force valid UUIDv4 version ('4') and variant ('8', '9', 'a', or 'b')
	b6 = (b6 & 0x0f) | 0x40 // High nibble of byte 6 is now 4
	b8 = (b8 & 0x3f) | 0x80 // High nibble of byte 8 is now 8

	// 5. Explicitly build the 36-byte array layout
	var buf [36]byte

	// --- Block 1: seed1 ---
	buf[0] = hex[b0>>4]
	buf[1] = hex[b0&0x0f]
	buf[2] = hex[b1>>4]
	buf[3] = hex[b1&0x0f]
	buf[4] = hex[b2>>4]
	buf[5] = hex[b2&0x0f]
	buf[6] = hex[b3>>4]
	buf[7] = hex[b3&0x0f]

	buf[8] = '-'

	buf[9] = hex[b4>>4]
	buf[10] = hex[b4&0x0f]
	buf[11] = hex[b5>>4]
	buf[12] = hex[b5&0x0f]

	buf[13] = '-'

	buf[14] = hex[b6>>4] // Always '4'
	buf[15] = hex[b6&0x0f]
	buf[16] = hex[b7>>4]
	buf[17] = hex[b7&0x0f]

	buf[18] = '-'

	// --- Block 2: seed2 ---
	buf[19] = hex[b8>>4] // Always '8'
	buf[20] = hex[b8&0x0f]
	buf[21] = hex[b9>>4]
	buf[22] = hex[b9&0x0f]

	buf[23] = '-'

	buf[24] = hex[b10>>4]
	buf[25] = hex[b10&0x0f]
	buf[26] = hex[b11>>4]
	buf[27] = hex[b11&0x0f]
	buf[28] = hex[b12>>4]
	buf[29] = hex[b12&0x0f]
	buf[30] = hex[b13>>4]
	buf[31] = hex[b13&0x0f]
	buf[32] = hex[b14>>4]
	buf[33] = hex[b14&0x0f]
	buf[34] = hex[b15>>4]
	buf[35] = hex[b15&0x0f]

	return string(buf[:])
}

func main() {
	// Small sequential IDs yield wildly distributed, non-repeating UUID strings
	fmt.Println("Seed 1:", UUIDFromUniqId(1))
	fmt.Println("Seed 2:", UUIDFromUniqId(2))
	fmt.Println("Seed 3:", UUIDFromUniqId(3))
	fmt.Println("Seed 0xebada5168620c5fe:", UUIDFromUniqId(0xebada5168620c5fe))

```

```
sudo mkdir --parents /tmp/dnv-dn-tmpfs
sudo mount --types tmpfs --options size=2G tmpfs /tmp/dnv-dn-tmpfs
sudo mkdir /tmp/dnv-dn-tmpfs/ebada5168620c5fe

sudo dd if=/dev/zero of=/tmp/dnv-dn-tmpfs/ebada5168620c5fe/000000000000001c-00000000000000f3 bs=1M count=12
sudo losetup /dev/loop11022 /tmp/dnv-dn-tmpfs/ebada5168620c5fe/000000000000001c-00000000000000f3

losetup --all --output NAME,BACK-FILE --json
{
   "loopdevices": [
      {
         "name": "/dev/loop109999",
         "back-file": "/tmp/dnv-dn-tmpfs/3c8a6914ec7b0e8e/ebada5168620c5fe-a6f94f65c5d03d93"
      }
   ]
}

sudo losetup --detach /dev/loop11022


sudo pvcreate --norestorefile --uuid f109ada1-b3dd-478e-b9ad-a809cfbabd8a --metadatasize 4M --dataalignment 8M /dev/loop0

lsblk --bytes --nodeps --json /dev/loop0
{
   "blockdevices": [
      {
         "name": "loop0",
         "maj:min": "7:0",
         "rm": false,
         "size": 524288000,
         "ro": false,
         "type": "loop",
         "mountpoints": [
             null
         ]
      }
   ]
}

sudo pvs --yes --report-format json --options pv_name,pv_uuid /dev/loop0
  {
      "report": [
          {
              "pv": [
                  {"pv_name":"/dev/loop0", "pv_uuid":"f109ad-a1b3-dd47-8eb9-ada8-09cf-babd8a"}
              ]
          }
      ]
  }

sudo vgcreate --yes --physicalextentsize 4M dnv-dn-ebada5168620c5fe /dev/loop0

sudo vgs --yes --report-format json --options vg_name dnv-dn-ebada5168620c5fe
  {
      "report": [
          {
              "vg": [
                  {"vg_name":"dnv-dn-ebada5168620c5fe"}
              ]
          }
      ]
  }

sudo lvcreate --yes --addtag not_trimmed --name 000000000000001c-00000000000000f3 --extents 120 dnv-dn-ebada5168620c5fe

sudo lvs --yes --report-format json --options lv_name,lv_tags /dev/dnv-dn-ebada5168620c5fe/000000000000001c-00000000000000f3
  {
      "report": [
          {
              "lv": [
                  {"lv_name":"000000000000001c-00000000000000f3", "lv_tags":"not_trimmed"}
              ]
          }
      ]
  }

sudo blkdiscard --force /dev/dnv-dn-ebada5168620c5fe/000000000000001c-00000000000000f3

sudo lvchange --yes --deltag not_trimmed --addtag trimmed /dev/dnv-dn-ebada5168620c5fe/000000000000001c-00000000000000f3

sudo lvs --yes --report-format json --options lv_name,lv_tags /dev/dnv-dn-ebada5168620c5fe/000000000000001c-00000000000000f3
  {
      "report": [
          {
              "lv": [
                  {"lv_name":"000000000000001c-00000000000000f3", "lv_tags":"trimmed"}
              ]
          }
      ]
  }

sudo lvremove --yes /dev/dnv-dn-ebada5168620c5fe/000000000000001c-00000000000000f3

sudo vgremove --yes dnv-dn-ebada5168620c5fe
sudo pvremove --yes /dev/loop0

```

```
dd if=/dev/zero of=/tmp/t0.img bs=1M count=1024
dd if=/dev/zero of=/tmp/t1.img bs=1M count=1024

sudo losetup /dev/loop240 /tmp/t0.img
sudo losetup /dev/loop241 /tmp/t1.img

udev rules:
for f in $(ls /usr/lib/udev/rules.d/ | grep -w 'md-raid'); do sudo ln -s /dev/null /etc/udev/rules.d/$f; done

sudo mdadm --create /dev/md/0123456789abebada5168620c5fe0fa0 --run --name dnv-cn-ebada5168620c5fe-0f-a0 --level=1 --raid-devices=2 --data-offset=2048K --bitmap=internal --bitmap-chunk=65536K --assume-clean /dev/loop240 /dev/loop241

sudo mdadm --stop /dev/md/0123456789abebada5168620c5fe0fa0

sudo mdadm --assemble /dev/md/0123456789abebada5168620c5fe0fa0 --name dnv-cn-ebada5168620c5fe-0f-a0 /dev/loop240 /dev/loop241

sudo mdadm --detail /dev/md/0123456789abebada5168620c5fe0fa0

sudo mdadm --manage /dev/md/0123456789abebada5168620c5fe0fa0 --add /dev/loop240


sudo dd if=/dev/zero of=/dev/loop240 bs=1M count=1 && sudo dd if=/dev/zero of=/dev/loop241 bs=1M count=1
sudo blkdiscard --force /dev/loop240 && sudo blkdiscard --force /dev/loop241



```