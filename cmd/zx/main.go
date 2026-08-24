// zx is a short alias for the zync CLI.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/notzree/zync/internal/commands"
)

func main() {
	if err := commands.NewRoot().Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
