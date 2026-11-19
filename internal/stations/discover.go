// Package stations pushes one flow file, and so one set of FlowLab diagrams, to
// the Flow installs a tenant runs in EmberNet, one station at a time, with a
// golden check after each and a revert of the one that fails.
//
// Which stations there are comes from EmberNet and from nowhere else: the
// dashboard's running apps for the tenant, the same list its Running Apps page
// shows. Nobody types a station in, and a station of another tenant is never
// used, whatever the dashboard sends.
package stations

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// AppName is the app a Flow install carries in the dashboard's list: the
// embernet.ai/app-name label of the Store's HotLoop Flow chart.
const AppName = "hotloop-flow"

// Station is one Flow install of a tenant.
type Station struct {
	// Name is the Store release, which is how the tenant knows the install.
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	// URL is where its admin API answers: its Flux intercept when the dashboard
	// reports one, and its cluster address otherwise.
	URL string `json:"url"`
	// Reach says which of the two URL is.
	Reach string `json:"reach"`
}

// Discovery is what the dashboard said a tenant runs.
type Discovery struct {
	Tenant   string    `json:"tenant"`
	Stations []Station `json:"stations"`
	// OtherTenants counts rows the dashboard sent for a tenant that was not asked
	// about. They are dropped; a count above zero is worth a look.
	OtherTenants int `json:"otherTenants,omitempty"`
}

// service is the part of the dashboard's ServiceInfo this reads
// (internal/k8s/services.go in industrial-dashboard).
type service struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	AppName     string `json:"appName"`
	TenantID    string `json:"tenantID"`
	GuiPort     int    `json:"guiPort"`
	HelmRelease string `json:"helmRelease"`
	Running     bool   `json:"running"`
	Access      *struct {
		ClusterIP string `json:"clusterIP"`
		Ports     []struct {
			Port int `json:"port"`
		} `json:"ports"`
		Flux *struct {
			Host string `json:"host"`
			Port int    `json:"port"`
		} `json:"flux"`
	} `json:"access"`
}

// Discover asks the dashboard for the tenant's running apps and keeps the Flow
// installs. The tenant is named by the caller and sent to the dashboard, which
// holds the person to their own tenants; the answer is checked again here and
// any row that is not the tenant's is dropped.
func Discover(ctx context.Context, hc *http.Client, dashboardURL, bearer, tenant string) (Discovery, error) {
	tenant = strings.TrimSpace(tenant)
	if tenant == "" {
		return Discovery{}, fmt.Errorf("no tenant: a push is always to one tenant's stations, named by you")
	}
	if strings.TrimSpace(dashboardURL) == "" {
		return Discovery{}, fmt.Errorf("no dashboard to ask which stations the tenant runs")
	}
	if bearer == "" {
		return Discovery{}, fmt.Errorf("no dashboard token: the dashboard decides which tenants you may see")
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	u := strings.TrimSuffix(dashboardURL, "/") + "/api/apps/services?tenant=" + url.QueryEscape(tenant)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Discovery{}, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	res, err := hc.Do(req)
	if err != nil {
		return Discovery{}, fmt.Errorf("asking the dashboard: %w", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return Discovery{}, err
	}
	switch {
	case res.StatusCode == http.StatusUnauthorized:
		return Discovery{}, fmt.Errorf("the dashboard did not accept the token")
	case res.StatusCode == http.StatusForbidden:
		return Discovery{}, fmt.Errorf("the dashboard says you may not see tenant %q: %s", tenant, strings.TrimSpace(string(body)))
	case res.StatusCode != http.StatusOK:
		return Discovery{}, fmt.Errorf("the dashboard answered %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	var rows []service
	if err := json.Unmarshal(body, &rows); err != nil {
		var wrapped struct {
			Services []service `json:"services"`
		}
		if json.Unmarshal(body, &wrapped) != nil {
			return Discovery{}, fmt.Errorf("the dashboard's list is not one we know: %w", err)
		}
		rows = wrapped.Services
	}
	d := Discovery{Tenant: tenant}
	seen := map[string]bool{}
	for _, s := range rows {
		if !strings.EqualFold(strings.TrimSpace(s.AppName), AppName) {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(s.TenantID), tenant) {
			d.OtherTenants++
			continue
		}
		if !s.Running {
			continue
		}
		st, ok := stationOf(s)
		if !ok || seen[st.URL] {
			continue
		}
		seen[st.URL] = true
		d.Stations = append(d.Stations, st)
	}
	sort.Slice(d.Stations, func(i, j int) bool { return d.Stations[i].Name < d.Stations[j].Name })
	if len(d.Stations) == 0 {
		return d, fmt.Errorf("tenant %q runs no HotLoop Flow that the dashboard shows as running", tenant)
	}
	return d, nil
}

func stationOf(s service) (Station, bool) {
	name := s.HelmRelease
	if name == "" {
		name = s.Name
	}
	st := Station{Name: name, Namespace: s.Namespace}
	if s.Access != nil && s.Access.Flux != nil && s.Access.Flux.Host != "" {
		port := s.Access.Flux.Port
		if port == 0 {
			port = s.GuiPort
		}
		if port == 0 {
			return Station{}, false
		}
		st.URL, st.Reach = "http://"+s.Access.Flux.Host+":"+strconv.Itoa(port), "flux"
		return st, true
	}
	if s.Access != nil && s.Access.ClusterIP != "" {
		port := s.GuiPort
		if port == 0 && len(s.Access.Ports) > 0 {
			port = s.Access.Ports[0].Port
		}
		if port == 0 {
			return Station{}, false
		}
		st.URL, st.Reach = "http://"+s.Access.ClusterIP+":"+strconv.Itoa(port), "cluster"
		return st, true
	}
	return Station{}, false
}
