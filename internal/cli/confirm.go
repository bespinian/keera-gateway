package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// confirmTyping makes somebody type a thing's name back before something
// irreversible happens to it. A reflex "y" does not catch the wrong row;
// typing the name does.
//
// noun is what the name is called ("alias", "name"), and undone says what did
// not happen when the answer is wrong.
func confirmTyping(noun, want, undone string) error {
	fmt.Printf("\n%s ", style.head("Type the "+noun+" to confirm:"))
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return fmt.Errorf("reading the confirmation: %w", err)
	}
	if strings.TrimSpace(line) != want {
		return fmt.Errorf("that is not the %s; %s", noun, undone)
	}
	return nil
}
