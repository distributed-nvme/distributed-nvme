//go:build race

package cnagent

// raceEnabled reports whether this test binary was built with the race
// detector. A test whose only observer is the detector
// (TestAConvergeDoesNotRaceAnotherCntlrsReads) skips without it.
const raceEnabled = true
