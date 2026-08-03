package config

import (
	"errors"
	"io"
	"os"
	"testing"
)

// useStdinInput swaps os.Stdin for a pipe pre-loaded with input, restoring
// the original on test cleanup. Mirrors kvv2's useStdinInput helper.
func useStdinInput(t *testing.T, input string) {
	t.Helper()

	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create stdin pipe: %v", err)
	}
	if _, err := w.WriteString(input); err != nil {
		t.Fatalf("failed to write stdin input: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("failed to close stdin writer: %v", err)
	}

	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = oldStdin
		_ = r.Close()
	})
}

// TestPrompt_LineFidelity proves the exact desync scenario from the bug
// report is fixed: "n\nsecret\n" must yield "n" then "secret" in order,
// with no line skipped or merged.
func TestPrompt_LineFidelity(t *testing.T) {
	useStdinInput(t, "n\nsecret\n")

	first, err := Prompt("first: ")
	if err != nil {
		t.Fatalf("first Prompt failed: %v", err)
	}
	if first != "n" {
		t.Fatalf("first = %q, want %q", first, "n")
	}

	second, err := Prompt("second: ")
	if err != nil {
		t.Fatalf("second Prompt failed: %v", err)
	}
	if second != "secret" {
		t.Fatalf("second = %q, want %q", second, "secret")
	}
}

func TestPrompt_EmptyLineReturnsEmptyString(t *testing.T) {
	useStdinInput(t, "\n")

	v, err := Prompt("label: ")
	if err != nil {
		t.Fatalf("Prompt failed: %v", err)
	}
	if v != "" {
		t.Fatalf("v = %q, want empty", v)
	}
}

func TestPromptRequired_RejectsEmptyUntilNonEmpty(t *testing.T) {
	useStdinInput(t, "\n\nsecret\n")

	v, err := PromptRequired("label: ")
	if err != nil {
		t.Fatalf("PromptRequired failed: %v", err)
	}
	if v != "secret" {
		t.Fatalf("v = %q, want %q", v, "secret")
	}
}

func TestPromptRequired_EOFOnAllEmpty(t *testing.T) {
	useStdinInput(t, "\n")

	_, err := PromptRequired("label: ")
	if err == nil {
		t.Fatalf("expected error on EOF, got nil")
	}
}

// TestPrompt_NoTrailingNewline proves the doc comment on Prompt: a final
// line with no trailing newline is returned with a nil error (partial input
// accepted), and the next call on the now-exhausted stdin reports io.EOF.
func TestPrompt_NoTrailingNewline(t *testing.T) {
	useStdinInput(t, "secret")

	v, err := Prompt("label: ")
	if err != nil {
		t.Fatalf("Prompt on no-trailing-newline input returned error: %v", err)
	}
	if v != "secret" {
		t.Fatalf("v = %q, want %q", v, "secret")
	}

	_, err = Prompt("label: ")
	if !errors.Is(err, io.EOF) {
		t.Fatalf("second Prompt error = %v, want io.EOF", err)
	}
}
