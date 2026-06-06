package main

import "testing"

func newTestEditor(text string) *Editor {
	return &Editor{
		buffer:           NewPieceTable(text),
		charWidth:        1,
		idealCursorX:     -1,
		savedAtUndoDepth: 0,
	}
}

func TestPieceTableInsertDelete(t *testing.T) {
	table := NewPieceTable("alpha\nbeta")

	table.Insert(5, []rune("\ngamma"))
	if got, want := table.String(), "alpha\ngamma\nbeta"; got != want {
		t.Fatalf("after insert got %q, want %q", got, want)
	}
	if got, want := table.LineCount(), 3; got != want {
		t.Fatalf("line count after insert got %d, want %d", got, want)
	}

	table.Delete(5, len([]rune("\ngamma")))
	if got, want := table.String(), "alpha\nbeta"; got != want {
		t.Fatalf("after delete got %q, want %q", got, want)
	}
	if got, want := table.LineCount(), 2; got != want {
		t.Fatalf("line count after delete got %d, want %d", got, want)
	}
}

func TestNewlineUndoRedo(t *testing.T) {
	editor := newTestEditor("abcd")
	editor.cursorX = 2

	editor.newline()
	if got, want := editor.buffer.String(), "ab\ncd"; got != want {
		t.Fatalf("after newline got %q, want %q", got, want)
	}

	editor.undo()
	if got, want := editor.buffer.String(), "abcd"; got != want {
		t.Fatalf("after undo got %q, want %q", got, want)
	}
	if editor.cursorX != 2 || editor.cursorY != 0 {
		t.Fatalf("cursor after undo got (%d,%d), want (2,0)", editor.cursorX, editor.cursorY)
	}

	editor.redo()
	if got, want := editor.buffer.String(), "ab\ncd"; got != want {
		t.Fatalf("after redo got %q, want %q", got, want)
	}
	if editor.cursorX != 0 || editor.cursorY != 1 {
		t.Fatalf("cursor after redo got (%d,%d), want (0,1)", editor.cursorX, editor.cursorY)
	}
}

func TestBackspaceJoinUndoRedo(t *testing.T) {
	editor := newTestEditor("ab\ncd")
	editor.cursorX = 0
	editor.cursorY = 1

	editor.backspace()
	if got, want := editor.buffer.String(), "abcd"; got != want {
		t.Fatalf("after backspace got %q, want %q", got, want)
	}

	editor.undo()
	if got, want := editor.buffer.String(), "ab\ncd"; got != want {
		t.Fatalf("after undo got %q, want %q", got, want)
	}

	editor.redo()
	if got, want := editor.buffer.String(), "abcd"; got != want {
		t.Fatalf("after redo got %q, want %q", got, want)
	}
}

func TestDeleteForwardJoinUndoRedo(t *testing.T) {
	editor := newTestEditor("ab\ncd")
	editor.cursorX = 2
	editor.cursorY = 0

	editor.deleteForward()
	if got, want := editor.buffer.String(), "abcd"; got != want {
		t.Fatalf("after delete got %q, want %q", got, want)
	}

	editor.undo()
	if got, want := editor.buffer.String(), "ab\ncd"; got != want {
		t.Fatalf("after undo got %q, want %q", got, want)
	}

	editor.redo()
	if got, want := editor.buffer.String(), "abcd"; got != want {
		t.Fatalf("after redo got %q, want %q", got, want)
	}
}
