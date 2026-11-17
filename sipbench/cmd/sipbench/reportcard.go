package main

import (
	"flag"
	"fmt"
	"os"

	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/reportcard"
	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/scenario"
)

// reportCard writes the report card (README.md, scenarios.csv, conformance.csv) from the JSON reports of a run
// on kind and, optionally, one in compose.
func reportCard(args []string) error {
	fs := flag.NewFlagSet("report-card", flag.ExitOnError)
	kindPath := fs.String("kind", "", "JSON report of the run through Kamailio on kind (required)")
	composePath := fs.String("compose", "", "JSON report of the compose run (optional)")
	scenDir := fs.String("scenarios", "scenarios", "scenario directory, for each scenario's description")
	confDir := fs.String("conformance", "conformance", "conformance record directory")
	commit := fs.String("commit", "", "the commit the runs were made from")
	out := fs.String("out", "report-card", "directory to write the card to")
	_ = fs.Parse(args)
	if *kindPath == "" {
		return fmt.Errorf("report-card: -kind is required")
	}

	in := reportcard.Input{Commit: *commit}
	var err error
	if in.Kind, err = reportcard.LoadReport(*kindPath); err != nil {
		return err
	}
	if *composePath != "" {
		if in.Compose, err = reportcard.LoadReport(*composePath); err != nil {
			return err
		}
	}
	if in.Scenarios, err = scenario.LoadDir(*scenDir); err != nil {
		return err
	}
	if in.Cases, err = reportcard.LoadConformance(*confDir); err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	if err := reportcard.Write(*out, in); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote the report card to %s\n", *out)
	return nil
}
