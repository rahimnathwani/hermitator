// Command hermitator hosts coding agents in gVisor sandboxes and connects
// them to WhatsApp.
//
//	hermitator serve                     run the service
//	hermitator user add EMAIL [--admin]  create a login (prints a password)
//	hermitator user passwd EMAIL         set a new password (prints it)
//	hermitator user list                 list logins
//	hermitator keygen                    print a new master key
//	hermitator doctor                    check that this host can run agents
//	hermitator version
//
// Configuration comes from HERMITATOR_* environment variables.
package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve()
	case "user":
		err = userCmd(os.Args[2:])
	case "keygen":
		err = keygen()
	case "doctor":
		err = doctor()
	case "version", "--version", "-v":
		fmt.Println("hermitator", version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "hermitator: unknown command %q\n\n", os.Args[1])
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hermitator:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: hermitator <command>

  serve                      run the service
  user add EMAIL [--admin]   create a login and print its password
  user passwd EMAIL          set a new random password and print it
  user list                  list logins
  keygen                     print a new master key for HERMITATOR_MASTER_KEY
  doctor                     check that this host can run agents
  version                    print the version
`)
	os.Exit(2)
}
