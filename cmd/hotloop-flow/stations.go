package main

// A push of one flow file, and so one set of FlowLab diagrams, to every Flow
// install a tenant runs in EmberNet. The list of stations comes from the
// EmberNet dashboard and is never typed in; see internal/stations.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-isatty"

	"github.com/HotLoop-io/hotloop-flow/internal/stations"
)

const stationsUsage = `usage: hotloop-flow stations list -tenant ID [-dashboard URL] [-dashboard-token-file FILE]
       hotloop-flow stations push -tenant ID -file flows.json [-dashboard URL] [-dashboard-token-file FILE]
                                  [-token-file FILE] [-note TEXT] [-dry-run] [-confirm-restart]
                                  [-window HH:MM-HH:MM] [-window-zone ZONE] [-settle 5s] [-json]

The stations are the HotLoop Flow installs EmberNet shows as running for the tenant, asked of
the dashboard with your dashboard token (HOTLOOP_FLOW_DASHBOARD_TOKEN, or -dashboard-token-file;
the address is HOTLOOP_FLOW_DASHBOARD_URL). The tenant is always named, and the dashboard holds
you to the tenants you may see.

push puts the file on each station in turn with the deploy token the stations accept
(HOTLOOP_FLOW_TOKEN, or -token-file; it needs flows.read, flows.write and status.read), checks each one afterwards, and if a station fails its
check puts that station back on what it had and stops. A push that would restart a running
logger stops first and asks: say yes at the prompt, pass -confirm-restart, or give the time
it is allowed in with -window. Exit status 0 means every station is done, 3 that a station
failed and the push stopped, 4 that it is waiting for you to agree to restarts, 5 that
stations were skipped (they did not answer, or hold no record to go back to).
`

func cmdStations(args []string, stdout io.Writer) error {
	if len(args) == 0 || (args[0] != "list" && args[0] != "push") {
		fmt.Fprint(os.Stderr, stationsUsage)
		return errors.New("stations needs list or push")
	}
	sub := args[0]
	fs := flag.NewFlagSet("stations "+sub, flag.ContinueOnError)
	dash := fs.String("dashboard", "", "the EmberNet dashboard (or HOTLOOP_FLOW_DASHBOARD_URL)")
	tenant := fs.String("tenant", "", "the tenant whose stations these are")
	dashTok := fs.String("dashboard-token-file", "", "read the dashboard token from this file")
	file := fs.String("file", "", "the flow file to push")
	tokenFile := fs.String("token-file", "", "read the stations' deploy token from this file")
	note := fs.String("note", "", "the note for each station's deployment log")
	dry := fs.Bool("dry-run", false, "plan every station and change none")
	confirm := fs.Bool("confirm-restart", false, "agree that running loggers may be restarted")
	window := fs.String("window", "", "the time of day a restart of loggers is agreed in advance, HH:MM-HH:MM")
	zone := fs.String("window-zone", "", "the zone of -window, an IANA name (default UTC)")
	settle := fs.Duration("settle", 5*time.Second, "how long a station's diagrams get to start before the check calls them not running")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	fs.Usage = func() { fmt.Fprint(fs.Output(), stationsUsage) }
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("stations takes flags only")
	}
	url := *dash
	if url == "" {
		url = os.Getenv("HOTLOOP_FLOW_DASHBOARD_URL")
	}
	bearer := os.Getenv("HOTLOOP_FLOW_DASHBOARD_TOKEN")
	if *dashTok != "" {
		b, err := os.ReadFile(*dashTok)
		if err != nil {
			return fmt.Errorf("reading the dashboard token file: %w", err)
		}
		bearer = strings.TrimSpace(string(b))
	}
	ctx := context.Background()
	hc := &http.Client{Timeout: 2 * time.Minute}
	found, err := stations.Discover(ctx, hc, url, bearer, *tenant)
	if err != nil {
		return err
	}
	if sub == "list" {
		if *asJSON {
			return json.NewEncoder(stdout).Encode(found)
		}
		for _, s := range found.Stations {
			fmt.Fprintf(stdout, "%s\t%s\t%s\n", s.Name, s.URL, s.Reach)
		}
		if found.OtherTenants > 0 {
			fmt.Fprintf(stdout, "(%d rows of other tenants were dropped)\n", found.OtherTenants)
		}
		return nil
	}

	if *file == "" {
		return errors.New("push needs -file")
	}
	doc, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	token := os.Getenv("HOTLOOP_FLOW_TOKEN")
	if *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			return fmt.Errorf("reading the token file: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}
	if token == "" {
		return errors.New("no deploy token for the stations: set HOTLOOP_FLOW_TOKEN, or pass -token-file")
	}
	w, err := stations.ParseWindow(*window, *zone)
	if err != nil {
		return err
	}
	opts := stations.Options{
		Tenant: found.Tenant, Stations: found.Stations, Flows: doc, Note: *note, Token: token,
		Confirm: *confirm, Window: w, Settle: *settle, DryRun: *dry, HTTP: hc,
	}
	if isTerminal(os.Stdin) && !*asJSON {
		opts.Ask = func(rs []stations.StationResult) bool {
			fmt.Fprintln(stdout, "This push would restart running loggers:")
			for _, r := range rs {
				fmt.Fprintf(stdout, "  %s: %s\n", r.Name, strings.Join(r.Restarts, ", "))
			}
			fmt.Fprint(stdout, "Restart them now? [y/N] ")
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			return strings.EqualFold(strings.TrimSpace(line), "y") || strings.EqualFold(strings.TrimSpace(line), "yes")
		}
	}
	rep, err := stations.Push(ctx, opts)
	if err != nil {
		return err
	}
	if *asJSON {
		if err := json.NewEncoder(stdout).Encode(rep); err != nil {
			return err
		}
	} else {
		writeStationReport(stdout, rep)
	}
	return reportExit(rep)
}

func reportExit(rep stations.Report) error {
	switch {
	case rep.NeedsConfirm:
		return exitCode(4)
	case rep.Stopped:
		return exitCode(3)
	}
	for _, r := range rep.Results {
		if r.Status == stations.Skipped {
			return exitCode(5)
		}
	}
	return nil
}

func writeStationReport(w io.Writer, rep stations.Report) {
	fmt.Fprintf(w, "Tenant %s, every station to end on revision %s.\n", rep.Tenant, rep.Rev)
	for _, r := range rep.Results {
		fmt.Fprintf(w, "  %-14s %s", r.Status, r.Name)
		if r.Detail != "" {
			fmt.Fprintf(w, ": %s", r.Detail)
		}
		if len(r.Restarts) > 0 {
			fmt.Fprintf(w, " (restarts %s)", strings.Join(r.Restarts, ", "))
		}
		fmt.Fprintln(w)
	}
	if rep.Reason != "" {
		fmt.Fprintf(w, "Stopped: %s\n", rep.Reason)
	}
}

// isTerminal says a person is at the other end. /dev/null is a character device
// too, so the mode bits are not enough.
func isTerminal(f *os.File) bool {
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}
