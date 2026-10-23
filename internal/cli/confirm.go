package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// confirm prints what is about to happen and makes somebody type the thing's
// name back before an irreversible step: a reflex "y" does not catch the
// wrong row. A bullet line starts with two spaces. noun is what the name is
// called ("alias", "name"); undone says what did not happen on a wrong
// answer. The prompt goes to stderr so `delete --json` stays valid JSON.
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
