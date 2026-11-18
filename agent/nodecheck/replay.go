package nodecheck

import (
	"context"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/agent/cnagent"
)

// replayName prefixes each result of the md replay.
const replayName = "md replay"

// replay runs the md replay of a controller node (cnagent.MdReplay) and
// adds its steps to the report, a result each: it refuses to start when the
// md assembly mask failed, since a node's own udev would then assemble the
// replay's legs behind it.
func (c *checker) replay(ctx context.Context, mask Result) {
	if mask.Level == LevelFail {
		c.rep.add(Result{Name: replayName, Level: LevelFail,
			Detail: "not started: the md assembly mask failed",
			Fix:    mask.Fix})
		return
	}
	rep := cnagent.MdReplay(ctx, c.env.Oc, c.opts.LocalStore)
	if rep.Refused != "" {
		c.rep.add(Result{Name: replayName, Level: LevelFail,
			Detail: "not started: " + rep.Refused,
			Fix:    "run the replay on a spare machine with no dnv agent"})
		return
	}
	for _, step := range rep.Steps {
		res := Result{Name: replayName + ": " + step.Name, Level: LevelOk,
			Detail: step.Detail}
		if !step.Ok {
			res.Level = LevelFail
			res.Fix = "this mdadm or kernel does not behave as the tested " +
				"ones: keep it off controller nodes, or report it"
		}
		c.rep.add(res)
	}
	if len(rep.Left) > 0 {
		c.rep.add(Result{Name: replayName + ": teardown", Level: LevelFail,
			Detail: "left: " + strings.Join(rep.Left, "; "),
			Fix:    "remove them by hand: mdadm --stop, losetup -d, rm -rf"})
	}
}
