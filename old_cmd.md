
```go

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


```shell
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

```shell
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