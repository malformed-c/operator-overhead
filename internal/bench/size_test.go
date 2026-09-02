package bench

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCountSourceClassifiesEachLineKind(t *testing.T) {
	const src = `package x

// a line comment
/* a block
   comment continues
   and ends here */
func f() int { // trailing comments count as CODE: the line carries a statement
	return 1 // and so does this
}
/* one-liner block */
`
	path := filepath.Join(t.TempDir(), "x.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := CountSource(path)
	if err != nil {
		t.Fatalf("CountSource: %v", err)
	}
	// comment: `// a line comment`, three block lines, `/* one-liner block */`
	if got.Comment != 5 {
		t.Errorf("Comment = %d, want 5", got.Comment)
	}
	// code: `package x`, `func f()...`, `return 1...`, `}`
	if got.Code != 4 {
		t.Errorf("Code = %d, want 4", got.Code)
	}
	if got.Blank != 1 {
		t.Errorf("Blank = %d, want 1", got.Blank)
	}
}

// A one-line `/* ... */` must not open a block that swallows the rest of the
// file — which would classify every remaining line as a comment and report a
// program of four lines.
func TestCountSourceDoesNotRunAwayOnAOneLineBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "y.ts")
	if err := os.WriteFile(path, []byte("/* hi */\nconst a = 1\nconst b = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := CountSource(path)
	if err != nil {
		t.Fatalf("CountSource: %v", err)
	}
	if got.Code != 2 || got.Comment != 1 {
		t.Errorf("got code=%d comment=%d, want 2 and 1", got.Code, got.Comment)
	}
}

// A missing file must be an error. Zero bytes and zero lines for an artifact
// nobody built is the smallest possible lie, and it would silently hand the
// size comparison to whichever arm was not built.
func TestCountSourceRefusesAMissingFile(t *testing.T) {
	if _, err := CountSource(filepath.Join(t.TempDir(), "absent.go")); err == nil {
		t.Fatal("a missing source file counted as zero lines")
	}
}
