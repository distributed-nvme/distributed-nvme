package common

import "unicode/utf8"

// TextCutMark is what a cut puts where it took text out: between the head
// and the end CutRowText keeps.
const TextCutMark = "..."

// rowTextHead is how much of a text's head CutRowText keeps: enough for the
// words an agent puts in front of a command's error, which say what failed,
// and one full-width dnv name of the object they name.
const rowTextHead = 128

// CutRowText cuts text to AgentTextMax bytes, the row text cap of
// architecture.md, Live-state reporting, by its middle (CutMiddle): a longer
// text keeps its first rowTextHead bytes, what failed and the object it
// names, and its end, where a command's cause is, with TextCutMark between.
// The agents cut every text they write into a row or beside one with it
// (agent.CutAgentText), and the gateway the text of its own refusal it
// lists beside the agent's in a forced start's dry run (gateway.md,
// Repairs and retired legs), so one cap and one cut hold every such text.
func CutRowText(text string) string {
	return CutMiddle(text, AgentTextMax, rowTextHead)
}

// CutMiddle cuts text to max bytes by its middle: it keeps its first head
// bytes and its end, TextCutMark between, never splitting a UTF-8 sequence.
// A text within max bytes is returned as it is.
func CutMiddle(text string, max int, head int) string {
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
