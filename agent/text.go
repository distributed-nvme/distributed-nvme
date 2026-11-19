package agent

import (
	"unicode/utf8"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// TextCutMark is what a cut puts where it took text out: in front of the
// end CutText keeps, and between the head and the end CutAgentText keeps.
const TextCutMark = "..."

// agentTextHead is how much of a text's head CutAgentText keeps: enough for
// the words an agent puts in front of a command's error, which say what
// failed, and one full-width dnv name of the object they name.
const agentTextHead = 128

// CutText is text cut to max bytes by its start, so the cause at its end
// stays: a cut text begins with TextCutMark, and a cut never splits a UTF-8
// sequence. A text within max bytes is returned as it is.
func CutText(text string, max int) string {
	if len(text) <= max {
		return text
	}
	keep := text[len(text)-(max-len(TextCutMark)):]
	for len(keep) > 0 && !utf8.RuneStart(keep[0]) {
		keep = keep[1:]
	}
	return TextCutMark + keep
}

// CutAgentText cuts text to common.AgentTextMax bytes, the row text cap of
// architecture.md, Live-state reporting (cutMiddle): a longer text keeps
// its first agentTextHead bytes, what failed and the object it names, and
// its end, where a command's cause is, with TextCutMark between, so what a
// cut takes out is the middle, a command line or its long output. Every
// text an agent writes into a row or beside one goes through it, so the
// agent's memory, its replies and the inspect replies carry one cut text,
// and two writers of one text, a forced start's row and its dry run, cut it
// alike. The agent's log record of a command (log.md, OS commands and file
// IO) keeps a command's text whole.
func CutAgentText(text string) string {
	return cutMiddle(text, common.AgentTextMax, agentTextHead)
}

// JoinNote puts note in front of text, on a line of its own, the note cut
// by its middle (cutMiddle) to the room text leaves within the row text cap,
// so the cap the row's writer keeps (CutAgentText) takes neither text, which
// a reader may parse, as the raw status line of a dm-clone row, nor the
// note's end, its cause. With less room than joinNoteMin the note stays
// whole, and the writer cuts the whole.
func JoinNote(note, text string) string {
	switch {
	case text == "":
		return note
	case note == "":
		return text
	}
	room := common.AgentTextMax - len(text) - len("\n")
	if len(note) > room && room >= joinNoteMin {
		note = cutMiddle(note, room, (room-len(TextCutMark))/2)
	}
	return note + "\n" + text
}

// joinNoteMin is the least room JoinNote cuts a note to.
const joinNoteMin = 32

// cutMiddle cuts text to max bytes by its middle: it keeps its first head
// bytes and its end, TextCutMark between, never splitting a UTF-8 sequence.
// A text within max bytes is returned as it is.
func cutMiddle(text string, max int, head int) string {
	if len(text) <= max {
		return text
	}
	for head > 0 && !utf8.RuneStart(text[head]) {
		head--
	}
	tail := len(text) - (max - head - len(TextCutMark))
	for tail < len(text) && !utf8.RuneStart(text[tail]) {
		tail++
	}
	return text[:head] + TextCutMark + text[tail:]
}
