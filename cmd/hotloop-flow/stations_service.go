package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/api"
	"github.com/HotLoop-io/hotloop-flow/internal/stations"
)

// stationsService is what the station routes and tools call: the settings say
// which tenant and which dashboard, and the two token files are read each time.
type stationsService struct {
	app *application
}

func (a *application) stationsService() api.StationsService {
	if !a.cfg.Stations.Enabled() {
		return nil
	}
	return &stationsService{app: a}
}

func readToken(path, what string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading the %s token: %w", what, err)
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", fmt.Errorf("the %s token file is empty", what)
	}
	return t, nil
}

func (s *stationsService) Discover(ctx context.Context) (stations.Discovery, error) {
	c := s.app.cfg.Stations
	bearer, err := readToken(c.DashboardTokenFile, "dashboard")
	if err != nil {
		return stations.Discovery{}, err
	}
	return stations.Discover(ctx, &http.Client{Timeout: time.Minute}, c.DashboardURL, bearer, c.Tenant)
}

func (s *stationsService) Push(ctx context.Context, req api.StationsPushRequest) (stations.Report, error) {
	c := s.app.cfg.Stations
	found, err := s.Discover(ctx)
	if err != nil {
		return stations.Report{}, err
	}
	token, err := readToken(c.DeployTokenFile, "deploy")
	if err != nil {
		return stations.Report{}, err
	}
	doc := []byte(req.Flows)
	if len(doc) == 0 {
		if doc = s.app.flowStore.Bytes(); doc == nil {
			return stations.Report{}, errors.New("this instance has no flow file to push, and the request carried none")
		}
	}
	w, err := stations.ParseWindow(req.Window, req.WindowZone)
	if err != nil {
		return stations.Report{}, err
	}
	return stations.Push(ctx, stations.Options{
		Tenant: found.Tenant, Stations: found.Stations, Flows: doc, Note: req.Note, Token: token,
		Confirm: req.ConfirmRestart, Window: w, DryRun: req.DryRun,
		HTTP: &http.Client{Timeout: 2 * time.Minute},
	})
}
