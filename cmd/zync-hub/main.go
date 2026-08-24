// zync-hub is the always-on coordination point: it serves bare git
// repositories over smart HTTP and arbitrates per-branch write leases.
package main

import (
	"go.uber.org/fx"
	_ "go.uber.org/automaxprocs"

	"github.com/notzree/zync/internal/hub"
)

func main() {
	fx.New(hub.Module).Run()
}
