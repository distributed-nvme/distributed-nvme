//go:build race

package dnagent

// raceEnabled reports whether this test binary was built with the race
// detector. The crash walks run every third cut under it
// (crashSampled), as they do under -short.
const raceEnabled = true
