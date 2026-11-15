package cli

// prompt.go — the minimal interactive picker: a numbered menu read as ONE
// line from stdin. Works on every terminal, over SSH, and in pipes (tests
// feed it a strings.Reader) — no raw-mode/arrow-key UI on purpose: zero
// dependencies beyond the standard library, and typing a digit is as fast
// as arrowing through the handful of hosts a machine knows.
import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// pickFromList prints the title plus numbered items and returns the chosen
// index. EOF — or a stream that never yields a number — is an error (the
// callers turn that into their non-interactive hint).
func pickFromList(stdout io.Writer, stdin io.Reader, title string, items []string) (int, error) {
	fmt.Fprintf(stdout, "%s\n", title)
	for i, it := range items {
		fmt.Fprintf(stdout, "  %d) %s\n", i+1, it)
	}
	fmt.Fprintf(stdout, "select 1-%d: ", len(items))
	rd := bufio.NewReader(stdin)
	for {
		line, err := rd.ReadString('\n')
		n, perr := strconv.Atoi(strings.TrimSpace(line))
		if perr == nil && n >= 1 && n <= len(items) {
			fmt.Fprintln(stdout)
			return n - 1, nil
		}
		if err != nil { // EOF/partial input without a valid number
			return -1, fmt.Errorf("no selection")
		}
		fmt.Fprintf(stdout, "enter a number 1-%d: ", len(items))
	}
}

// promptLine reads one free-form line (empty/EOF → error).
func promptLine(stdout io.Writer, stdin io.Reader, label string) (string, error) {
	fmt.Fprint(stdout, label)
	rd := bufio.NewReader(stdin)
	line, _ := rd.ReadString('\n')
	v := strings.TrimSpace(line)
	if v == "" {
		return "", fmt.Errorf("no input")
	}
	return v, nil
}
