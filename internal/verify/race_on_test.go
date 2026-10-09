//go:build race

package verify

// raceEnabled is true when the race detector is instrumenting this build.
// It instruments every allocation, which makes an allocation-ratio
// measurement meaningless — observed at about 530x the log size against a
// limit of 4 — so the allocation bound is asserted without it.
const raceEnabled = true
