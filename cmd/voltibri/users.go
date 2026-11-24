// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/voltibri/voltibri/internal/auth"
	"github.com/voltibri/voltibri/internal/store"
)

const usersUsage = `Usage:
  voltibri users list    [-data DIR]
  voltibri users add     [-data DIR] [-admin] [-name NAME] LOGIN
  voltibri users invite  [-data DIR] LOGIN
  voltibri users logout  [-data DIR] LOGIN

Emergency access on the machine voltibri runs on. "add" creates a user and
"invite" (also for a forgotten password) prints a one-time link with which
the user sets a password; open it on the voltibri address, e.g.
http://voltibri.local:7070/login?invite=... "logout" ends all sessions.
`

func usersCmd(args []string) error {
	if len(args) == 0 {
		fmt.Print(usersUsage)
		return errors.New("subcommand missing")
	}
	fs := flag.NewFlagSet("users "+args[0], flag.ExitOnError)
	dataDir := fs.String("data", defaultDataDir(), "data directory, env VOLTIBRI_DATA")
	admin := fs.Bool("admin", false, "administrator")
	name := fs.String("name", "", "display name")
	fs.Parse(args[1:])
	st, err := store.Open(*dataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	svc := auth.NewService(st)
	login := fs.Arg(0)
	find := func() (auth.User, error) {
		u, _, err := st.UserByLogin(ctx, login)
		if err != nil {
			return u, fmt.Errorf("user %q: %w", login, err)
		}
		return u, nil
	}
	invite := func(u auth.User) error {
		token, exp, err := svc.Invite(ctx, u.ID)
		if err != nil {
			return err
		}
		fmt.Printf("Link for %s (valid until %s):\n  /login?invite=%s\n", u.Login, exp.Format("2006-01-02 15:04"), url.QueryEscape(token))
		return nil
	}
	switch args[0] {
	case "list":
		us, err := st.Users(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		defer tw.Flush()
		fmt.Fprintln(tw, "LOGIN\tNAME\tROLE\tGROUPS\tPASSWORD\tLAST SEEN")
		for _, u := range us {
			seen := "-"
			if !u.LastSeen.IsZero() {
				seen = u.LastSeen.Format("2006-01-02 15:04")
			}
			role := u.Role
			if u.Disabled {
				role += " (disabled)"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%v\t%s\n", u.Login, u.Name, role, strings.Join(u.Groups, ","), u.HasPassword, seen)
		}
		return nil
	case "add":
		if login == "" {
			return errors.New("login missing")
		}
		role := auth.RoleUser
		if *admin {
			role = auth.RoleAdmin
		}
		u, err := svc.SaveUser(ctx, auth.User{Login: login, Name: *name, Role: role})
		if err != nil {
			return err
		}
		return invite(u)
	case "invite":
		u, err := find()
		if err != nil {
			return err
		}
		return invite(u)
	case "logout":
		u, err := find()
		if err != nil {
			return err
		}
		return st.DeleteSessions(ctx, u.ID)
	}
	fmt.Print(usersUsage)
	return fmt.Errorf("unknown subcommand %q", args[0])
}
