package wf

import ()

// appendNote adds one note after another, never over it.
func appendNote(note, add string) string {
	switch {
	case add == "":
		return note
	case note == "":
		return add
	}
	return note + " " + add
}
