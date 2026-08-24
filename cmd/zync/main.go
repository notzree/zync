// zync is the replica-side CLI: enroll repos, take and hand off write
// leases, and watch the whole system from a TUI.
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
