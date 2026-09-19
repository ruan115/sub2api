// Command fixture prepares credentials and schema for disposable integration
// dependencies. It does not connect to any database or start any service.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
)

type config struct {
	output      string
	ccmaxSource string
	mysqlPort   int
	redisPort   int
}

func run(args []string, stdout, stderr io.Writer) error {
	var cfg config
	flags := flag.NewFlagSet("fixture", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&cfg.output, "output", "", "new private output directory (must not exist)")
	flags.StringVar(&cfg.ccmaxSource, "ccmax-schema-source", "", "path to ccmax-manager/execution_outbox.go")
	flags.IntVar(&cfg.mysqlPort, "mysql-port", 33379, "loopback MySQL test port")
	flags.IntVar(&cfg.redisPort, "redis-port", 63979, "loopback Redis test port")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("positional arguments are not supported")
	}
	if err := generate(cfg); err != nil {
		return err
	}
	_, err := fmt.Fprintln(stdout, "Created private dependency fixtures; credentials were not printed.")
	return err
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "fixture: %v\n", err)
		os.Exit(1)
	}
}
