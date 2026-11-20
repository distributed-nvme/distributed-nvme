// Package nodecheck is the node check and the gather of `dnv-agent`
// (dnagent.md CM5, CM6): `dnv-agent check dn|cn` reads, read-only but for
// the cn's opt-in md replay, the facts an agent's work and dnv's safety rest
// on, and reports each with a fix line for what fails or warns; `dnv-agent
// collect` gathers a node's state into one archive for a bug report. Neither
// starts a server, and an agent never runs either.
package nodecheck

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Level is how a result of the node check stands (dnagent.md CM5): a
// failure where the agent cannot work or safety rests on the fact; a
// warning for one of four kinds: a node setting the check warns for rather
// than fails, which must still be set (lvm2, mdadm.conf, the modules loaded
// at boot, a controller node's kernel before 6.19, where every controller
// node must run one kernel series, a disk node's nvme_core.io_timeout at
// the value that never times out); a known limit (a disk whose IO size is
// above what nvmet-tcp takes, a controller node whose host cut row finds a
// cause, checked with --ignore-host-disconnection); a fact outside what dnv
// is tested with; or a fact the check could not read, whose detail starts
// "not read".
type Level string

const (
	LevelOk   Level = "ok"
	LevelWarn Level = "warn"
	LevelFail Level = "fail"
)

// Result is one read of the node check: what it read, how that stands, and,
// for a warning or a failure, what to do about it.
type Result struct {
	Name   string `json:"name"`
	Level  Level  `json:"level"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// Report is the node check's answer: the role it checked and each result,
// in the order read, with the count of failures and of warnings.
type Report struct {
	Role     string   `json:"role"`
	Results  []Result `json:"results"`
	Failures int      `json:"failures"`
	Warnings int      `json:"warnings"`
}

func (r *Report) add(res Result) {
	r.Results = append(r.Results, res)
	switch res.Level {
	case LevelFail:
		r.Failures++
	case LevelWarn:
		r.Warnings++
	}
}

// The exit statuses of the node check and the gather (dnagent.md CM5, CM6):
// ExitOk when nothing failed, warnings allowed; ExitFailed when a result
// failed; ExitNotRun when the tool could not run at all, as when not root.
const (
	ExitOk     = 0
	ExitFailed = 1
	ExitNotRun = 2
)

// ExitError ends a node check or a gather with an exit status of its own,
// in place of the command line's error print: Err, when set, is what main
// prints on stderr; a report the tool printed already says the rest.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("exit status %d", e.Code)
}

func (e *ExitError) Unwrap() error { return e.Err }

// notRun is the ExitError of a tool that could not run.
func notRun(format string, args ...any) *ExitError {
	return &ExitError{Code: ExitNotRun, Err: fmt.Errorf(format, args...)}
}

// The output forms of the node check: text for a person, json for the
// suites.
const (
	OutputText = "text"
	OutputJson = "json"
)

// write prints the report in the form output names.
func (r *Report) write(w io.Writer, output string) error {
	if output == OutputJson {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "dnv-agent check %s\n", r.Role)
	for _, res := range r.Results {
		fmt.Fprintf(&b, "%-5s %s: %s\n", levelWord(res.Level), res.Name,
			res.Detail)
		if res.Fix != "" {
			fmt.Fprintf(&b, "      fix: %s\n", res.Fix)
		}
	}
	fmt.Fprintf(&b, "%d %s, %d %s\n", r.Failures,
		plural(r.Failures, "failure"), r.Warnings,
		plural(r.Warnings, "warning"))
	_, err := io.WriteString(w, b.String())
	return err
}

// levelWord is a level as the text form prints it, the two that need a fix
// in capitals.
func levelWord(level Level) string {
	switch level {
	case LevelFail:
		return "FAIL"
	case LevelWarn:
		return "WARN"
	}
	return "ok"
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// exitCode is the exit status the report ends the check with.
func (r *Report) exitCode() int {
	if r.Failures > 0 {
		return ExitFailed
	}
	return ExitOk
}
