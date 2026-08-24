// Package commands defines the zync CLI.
package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/urfave/cli/v3"

	"github.com/notzree/zync/internal/cliconf"
	"github.com/notzree/zync/internal/ops"
	"github.com/notzree/zync/internal/tui"
)

func newOps() (*ops.Ops, error) {
	g, err := cliconf.LoadGlobal()
	if err != nil {
		return nil, err
	}
	return ops.New(g), nil
}

func cwd() string {
	dir, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return dir
}

func NewRoot() *cli.Command {
	name := "zync"
	if len(os.Args) > 0 && os.Args[0] != "" {
		name = filepath.Base(os.Args[0])
	}
	return &cli.Command{
		Name:  name,
		Usage: "git-based codebase handoffs between your machines and your homeserver",
		Commands: []*cli.Command{
			{
				Name:  "setup",
				Usage: "configure this replica (hub URL, token, replica name)",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "hub", Usage: "hub base URL, e.g. http://zync.homelab:8080", Required: true},
					&cli.StringFlag{Name: "token", Usage: "hub auth token", Required: true},
					&cli.StringFlag{Name: "name", Usage: "name of this replica, e.g. laptop", Required: true},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					g := cliconf.Global{HubURL: c.String("hub"), Token: c.String("token"), Replica: c.String("name")}
					if err := cliconf.SaveGlobal(g); err != nil {
						return err
					}
					fmt.Printf("configured replica %q against %s\n", g.Replica, g.HubURL)
					return nil
				},
			},
			{
				Name:  "init",
				Usage: "enroll the current repo as a workspace and take the lease on the current branch",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "workspace", Usage: "workspace name (default: directory name)"},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					o, err := newOps()
					if err != nil {
						return err
					}
					ws, err := o.Init(cwd(), c.String("workspace"))
					if err != nil {
						return err
					}
					fmt.Printf("workspace %q enrolled; this replica holds the lease on the current branch\n", ws)
					return nil
				},
			},
			{
				Name:      "clone",
				Usage:     "clone a workspace from the hub onto this replica",
				ArgsUsage: "<workspace> [path]",
				Action: func(ctx context.Context, c *cli.Command) error {
					if c.Args().Len() < 1 {
						return fmt.Errorf("usage: zync clone <workspace> [path]")
					}
					o, err := newOps()
					if err != nil {
						return err
					}
					dest, err := o.Clone(c.Args().Get(0), c.Args().Get(1))
					if err != nil {
						return err
					}
					fmt.Printf("cloned workspace %q into %s (read-only until you `zync take`)\n", c.Args().Get(0), dest)
					return nil
				},
			},
			{
				Name:      "take",
				Usage:     "acquire the write lease and sync to the latest flushed state",
				ArgsUsage: "[branch]",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "force", Usage: "break a lease held by another replica (their generation is fenced out)"},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					o, err := newOps()
					if err != nil {
						return err
					}
					if err := o.Take(cwd(), c.Args().Get(0), c.Bool("force")); err != nil {
						return err
					}
					fmt.Println("lease acquired; this replica is now mutable")
					return nil
				},
			},
			{
				Name:      "handoff",
				Usage:     "flush the full working state to the hub and release the lease",
				ArgsUsage: "[branch]",
				Action: func(ctx context.Context, c *cli.Command) error {
					o, err := newOps()
					if err != nil {
						return err
					}
					if err := o.Handoff(cwd(), c.Args().Get(0)); err != nil {
						return err
					}
					fmt.Println("handed off; this replica is now read-only for that branch")
					return nil
				},
			},
			{
				Name:  "ls",
				Usage: "list workspace names (one per line; used by agent bootstrap scripts)",
				Action: func(ctx context.Context, c *cli.Command) error {
					o, err := newOps()
					if err != nil {
						return err
					}
					wss, err := o.Client.ListWorkspaces()
					if err != nil {
						return err
					}
					for _, ws := range wss {
						fmt.Println(ws.Name)
					}
					return nil
				},
			},
			{
				Name:  "status",
				Usage: "show all leases and this replica's local state",
				Action: func(ctx context.Context, c *cli.Command) error {
					o, err := newOps()
					if err != nil {
						return err
					}
					leases, err := o.Client.ListLeases()
					if err != nil {
						return err
					}
					w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
					fmt.Fprintln(w, "WORKSPACE\tBRANCH\tSTATE\tHOLDER\tGEN\tUPDATED")
					for _, l := range leases {
						holder := l.Holder
						if holder == o.Client.Replica() {
							holder += " (this replica)"
						}
						fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n", l.Workspace, l.Branch, l.State, holder, l.Generation, l.UpdatedAt)
					}
					w.Flush()

					if ls, err := o.LocalStatus(cwd()); err == nil && ls != nil {
						fmt.Printf("\nlocal: workspace=%s branch=%s", ls.Workspace, ls.Branch)
						switch {
						case ls.Holding:
							fmt.Print(" [MUTABLE - you hold the lease]")
						case ls.Diverged:
							fmt.Print(" [DIVERGED - working tree changed without the lease!]")
						default:
							fmt.Print(" [read-only]")
						}
						fmt.Println()
					}
					return nil
				},
			},
			{
				Name:  "tui",
				Usage: "interactive dashboard of workspaces and leases",
				Action: func(ctx context.Context, c *cli.Command) error {
					o, err := newOps()
					if err != nil {
						return err
					}
					return tui.Run(o)
				},
			},
		},
	}
}
