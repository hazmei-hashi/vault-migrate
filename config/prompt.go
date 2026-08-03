package config

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// ponytail: one shared bufio.Reader over os.Stdin for the whole process.
// Multiple bufio readers over the same os.Stdin each buffer-ahead and
// silently drop whatever bytes a sibling reader already consumed into its
// own buffer. Mixing fmt.Scan (token-oriented, splits on whitespace) with a
// second reader is worse: fmt.Scan swallows an entire line as "one token",
// desyncing every subsequent line-based prompt by one line. Routing every
// prompt through this single reader keeps stdin consumption in lockstep
// with what's printed.
//
// stdinSrc/stdin are rebuilt if os.Stdin changes identity (tests swap it
// via a pipe); real runs never swap os.Stdin so this is a no-op there.
var (
	stdinSrc io.Reader = os.Stdin
	stdin              = bufio.NewReader(os.Stdin)
)

// Prompt prints label, reads one line from stdin, and returns it trimmed.
// A final line with no trailing newline is returned with a nil error (partial
// input accepted); the next call then reports io.EOF. An error is only
// returned when nothing was read.
func Prompt(label string) (string, error) {
	if stdinSrc != os.Stdin {
		stdinSrc = os.Stdin
		stdin = bufio.NewReader(os.Stdin)
	}
	fmt.Print(label)
	line, err := stdin.ReadString('\n')
	line = strings.TrimSpace(line)
	if err != nil && line == "" {
		return "", err
	}
	return line, nil
}

// PromptRequired repeats Prompt until a non-empty value is entered.
func PromptRequired(label string) (string, error) {
	for {
		value, err := Prompt(label)
		if err != nil {
			return "", err
		}
		if value != "" {
			return value, nil
		}
		fmt.Println("  value required")
	}
}
