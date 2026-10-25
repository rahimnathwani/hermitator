package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/rahimnathwani/hermitator/internal/auth"
	"github.com/rahimnathwani/hermitator/internal/config"
	"github.com/rahimnathwani/hermitator/internal/hostops"
	"github.com/rahimnathwani/hermitator/internal/secrets"
	"github.com/rahimnathwani/hermitator/internal/store"
)

func openStore(ctx context.Context) (*store.Store, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	return store.Open(ctx, cfg.DBPath())
}

// password returns one read from stdin if --password-stdin was given, or a
// random one.
func password(fromStdin bool) (string, bool, error) {
	if fromStdin {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", false, err
		}
		return strings.TrimRight(line, "\r\n"), false, nil
	}
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	var b strings.Builder
	for i := 0; i < 20; i++ {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b.WriteByte(alphabet[n.Int64()])
	}
	return b.String(), true, nil
}

func userCmd(args []string) error {
	ctx := context.Background()
	if len(args) == 0 {
		return errors.New("usage: hermitator user add|passwd|list")
	}
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("user add", flag.ContinueOnError)
		admin := fs.Bool("admin", false, "create an administrator")
		stdin := fs.Bool("password-stdin", false, "read the password from stdin")
		maxAgents := fs.Int("max-agents", 5, "agent limit for a regular user")
		email, rest := splitFirst(args[1:])
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if email == "" {
			return errors.New("usage: hermitator user add EMAIL [--admin] [--password-stdin]")
		}
		pw, generated, err := password(*stdin)
		if err != nil {
			return err
		}
		hash, err := auth.HashPassword(pw)
		if err != nil {
			return err
		}
		role := store.RoleUser
		if *admin {
			role = store.RoleAdmin
		}
		u := &store.User{Email: email, PasswordHash: hash, Role: role, MaxAgents: *maxAgents}
		if err := st.CreateUser(ctx, u); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return fmt.Errorf("%s already exists", email)
			}
			return err
		}
		st.Audit(ctx, "cli", u.ID, "user.create", u.Email+" role="+role)
		fmt.Printf("created %s %s\n", role, u.Email)
		if generated {
			fmt.Printf("password: %s\n", pw)
		}
	case "passwd":
		fs := flag.NewFlagSet("user passwd", flag.ContinueOnError)
		stdin := fs.Bool("password-stdin", false, "read the password from stdin")
		email, rest := splitFirst(args[1:])
		if err := fs.Parse(rest); err != nil {
			return err
		}
		u, err := st.UserByEmail(ctx, email)
		if err != nil {
			return fmt.Errorf("no user %s", email)
		}
		pw, generated, err := password(*stdin)
		if err != nil {
			return err
		}
		hash, err := auth.HashPassword(pw)
		if err != nil {
			return err
		}
		if err := st.SetPassword(ctx, u.ID, hash); err != nil {
			return err
		}
		st.Audit(ctx, "cli", u.ID, "user.password_reset", u.Email)
		fmt.Printf("password changed for %s; their sessions were ended\n", u.Email)
		if generated {
			fmt.Printf("password: %s\n", pw)
		}
	case "list":
		users, err := st.Users(ctx)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "EMAIL\tROLE\tAGENTS\tSUSPENDED\tCREATED")
		for _, u := range users {
			n, _ := st.CountAgents(ctx, u.ID)
			fmt.Fprintf(w, "%s\t%s\t%d/%d\t%v\t%s\n", u.Email, u.Role, n, u.MaxAgents, u.Suspended, u.CreatedAt.Format("2006-01-02"))
		}
		w.Flush()
	default:
		return fmt.Errorf("unknown user command %q", args[0])
	}
	return nil
}

// splitFirst separates a leading positional argument from flags, so
// `user add EMAIL --admin` and `user add --admin EMAIL` both work.
func splitFirst(args []string) (string, []string) {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a, append(append([]string{}, args[:i]...), args[i+1:]...)
		}
	}
	return "", args
}

func keygen() error {
	fmt.Println(secrets.GenerateKey())
	return nil
}

// doctor checks the host and prints what it finds.
func doctor() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	fmt.Println("hermitator", version)
	checks := hostops.Doctor(context.Background(), cfg)
	for _, c := range checks {
		switch {
		case c.Note != "":
			fmt.Printf("  --    %s: %s\n", c.Name, c.Note)
		case c.Err == nil:
			fmt.Printf("  ok    %s\n", c.Name)
		default:
			fmt.Printf("  FAIL  %s: %v\n", c.Name, c.Err)
			if c.Hint != "" {
				fmt.Printf("        %s\n", c.Hint)
			}
		}
	}
	if hostops.Failed(checks) {
		return errors.New("some checks failed")
	}
	return nil
}
