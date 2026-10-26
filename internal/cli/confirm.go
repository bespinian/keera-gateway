package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// confirm makes somebody type the thing's name back before an irreversible
// step, since a reflex "y" does not catch the wrong row. undone says what did
// not happen on a wrong answer. It writes to stderr so `delete --json` stays
// valid JSON.
func confirm(title string, lines []string, noun, want, undone string) error {
	fmt.Fprintln(os.Stderr, styleErr.head(title))
	for _, line := range lines {
		fmt.Fprintln(os.Stderr, line)
	}
	fmt.Fprintf(os.Stderr, "\n%s ", styleErr.head("Type the "+noun+" to confirm:"))
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return fmt.Errorf("reading the confirmation: %w", err)
	}
	if strings.TrimSpace(line) != want {
		return fmt.Errorf("that is not the %s; %s", noun, undone)
	}
	return nil
}
