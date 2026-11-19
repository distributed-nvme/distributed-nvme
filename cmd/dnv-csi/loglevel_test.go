package main

import (
	"log/slog"
	"testing"

	"github.com/spf13/viper"
)

// log.md R6: both plugins take --log-level, debug, info or warn, from the
// flag or the environment, with info by default; any other word is refused
// before the start, naming the words.
func TestLogLevel(t *testing.T) {
	parse(t, exampleArgs)
	if got, err := logLevelFromViper(); err != nil || got != slog.LevelInfo {
		t.Errorf("default log level %v, %v, want INFO", got, err)
	}
	parse(t, append(append([]string{}, exampleArgs...), "--log-level", "debug"))
	if got, err := logLevelFromViper(); err != nil || got != slog.LevelDebug {
		t.Errorf("--log-level debug gave %v, %v", got, err)
	}
	t.Setenv("DNV_CSI_LOG_LEVEL", "warn")
	parse(t, exampleArgs)
	if got, err := logLevelFromViper(); err != nil || got != slog.LevelWarn {
		t.Errorf("DNV_CSI_LOG_LEVEL=warn gave %v, %v", got, err)
	}
	parse(t, append(append([]string{}, exampleArgs...), "--log-level", "error"))
	_, err := logLevelFromViper()
	if want := `--log-level must be debug, info or warn, got "error"`; err == nil ||
		err.Error() != want {
		t.Errorf("--log-level error: error %v, want %q", err, want)
	}

	// The node plugin takes the same flag.
	viper.Reset()
	node := newNodeCmd()
	if err := node.ParseFlags([]string{"--log-level", "debug"}); err != nil {
		t.Fatalf("node --log-level: %v", err)
	}
	if err := bindViper(node); err != nil {
		t.Fatalf("bindViper: %v", err)
	}
	if got, err := logLevelFromViper(); err != nil || got != slog.LevelDebug {
		t.Errorf("node --log-level debug gave %v, %v", got, err)
	}
}
