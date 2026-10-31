package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

// This acceptance test uses a local mock controller, never a real device.
func TestAccDeviceSwitchOperationalSpeed(t *testing.T) {
	var speed atomic.Int64
	speed.Store(1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var result any
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/info":
			result = map[string]any{"omadacId": "controller"}
		case r.Method == http.MethodPost && r.URL.Path == "/controller/api/v2/login":
			result = map[string]any{"token": "test-token"}
		case r.Method == http.MethodGet && r.URL.Path == "/controller/api/v2/sites/site/switches/02-00-00-00-00-01":
			// A port configured for auto negotiation (linkSpeed=0) can report
			// different operational speeds without any configuration change.
			linkStatus := 1
			if speed.Load() == 0 {
				linkStatus = 0
			}
			result = map[string]any{
				"mac": "02-00-00-00-00-01", "name": "Test-Switch", "ledSetting": 2,
				"ipSetting": map[string]any{"mode": "dhcp"},
				"ports": []map[string]any{{
					"port": 7, "name": "Test-Port", "speed": speed.Load(), "linkSpeed": 0,
					"portStatus": map[string]any{"linkStatus": linkStatus, "linkSpeed": speed.Load()},
				}},
			}
		default:
			// In particular, no configuration writes should be attempted.
			t.Errorf("unexpected controller request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"errorCode": 0, "result": result}); err != nil {
			t.Errorf("encoding controller response: %v", err)
		}
	}))
	defer server.Close()

	config := fmt.Sprintf(`
provider "omada" {
  url      = %q
  username = "test"
  password = "test"
}

resource "omada_device_switch" "test" {
  name = "Test-Switch"
  ports = [{ port = 7, name = "Test-Port" }]
}
`, server.URL)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config + `
import {
  to = omada_device_switch.test
  id = "site/02-00-00-00-00-01"
}
`,
				Check: resource.TestCheckResourceAttr("omada_device_switch.test", "ports.0.speed", "1"),
			},
			{
				PreConfig: func() { speed.Store(3) },
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttr("omada_device_switch.test", "ports.0.speed", "3"),
			},
			{
				PreConfig: func() { speed.Store(0) },
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: resource.TestCheckResourceAttr("omada_device_switch.test", "ports.0.speed", "0"),
			},
		},
	})
}

// This mock-controller regression test guards the invariant that operational
// speed is never sent as configured port linkSpeed during an intentional update.
// It passes with or without the computed-only schema change in PR #23.
func TestAccDeviceSwitchOperationalSpeedDuringPortUpdate(t *testing.T) {
	var speed atomic.Int64
	speed.Store(1)
	var renamed atomic.Bool
	var portUpdateCount atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var result any
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/info":
			result = map[string]any{"omadacId": "controller"}
		case r.Method == http.MethodPost && r.URL.Path == "/controller/api/v2/login":
			result = map[string]any{"token": "test-token"}
		case r.Method == http.MethodGet && r.URL.Path == "/controller/api/v2/sites/site/switches/02-00-00-00-00-01":
			portName := "Test-Port"
			if renamed.Load() {
				portName = "Renamed-Port"
			}
			result = map[string]any{
				"mac": "02-00-00-00-00-01", "name": "Test-Switch", "ledSetting": 2,
				"ipSetting": map[string]any{"mode": "dhcp"},
				"ports": []map[string]any{{
					"port": 7, "name": portName, "speed": speed.Load(), "linkSpeed": 0,
					"portStatus": map[string]any{"linkStatus": 1, "linkSpeed": speed.Load()},
				}},
			}
		case r.Method == http.MethodPut && r.URL.Path == "/controller/api/v2/sites/site/switches/02-00-00-00-00-01/config/general":
			// The resource updates switch-level settings before the port PATCH.
			result = map[string]any{}
		case r.Method == http.MethodPut && r.URL.Path == "/controller/api/v2/sites/site/switches/02-00-00-00-00-01/config/service":
			result = map[string]any{}
		case r.Method == http.MethodPatch && r.URL.Path == "/controller/api/v2/sites/site/switches/02-00-00-00-00-01/ports/7":
			var portConfig map[string]any
			if err := json.NewDecoder(r.Body).Decode(&portConfig); err != nil {
				t.Errorf("decoding port update: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if got := portConfig["name"]; got != "Renamed-Port" {
				t.Errorf("port update name = %v, want Renamed-Port", got)
			}
			for _, field := range []string{"speed", "linkSpeed"} {
				if got, ok := portConfig[field]; ok {
					t.Errorf("port update unexpectedly contains %s=%v; operational speed must not be configured", field, got)
				}
			}
			portUpdateCount.Add(1)
			renamed.Store(true)
			// Change operational speed after planning, during the write itself.
			speed.Store(3)
			result = map[string]any{}
		default:
			t.Errorf("unexpected controller request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"errorCode": 0, "result": result}); err != nil {
			t.Errorf("encoding controller response: %v", err)
		}
	}))
	defer server.Close()

	config := fmt.Sprintf(`
provider "omada" {
  url      = %q
  username = "test"
  password = "test"
}

resource "omada_device_switch" "test" {
  name = "Test-Switch"
  ports = [{ port = 7, name = "%%s" }]
}
`, server.URL)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(config, "Test-Port") + `
import {
  to = omada_device_switch.test
  id = "site/02-00-00-00-00-01"
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omada_device_switch.test", "ports.0.name", "Test-Port"),
					resource.TestCheckResourceAttr("omada_device_switch.test", "ports.0.speed", "1"),
				),
			},
			{
				Config: fmt.Sprintf(config, "Renamed-Port"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("omada_device_switch.test", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omada_device_switch.test", "ports.0.name", "Renamed-Port"),
					resource.TestCheckResourceAttr("omada_device_switch.test", "ports.0.speed", "3"),
				),
			},
			{
				Config: fmt.Sprintf(config, "Renamed-Port"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
	if got := portUpdateCount.Load(); got != 1 {
		t.Errorf("mock controller observed %d port updates, want exactly 1", got)
	}
}

func TestAccDeviceSwitchRejectConfiguredSpeed(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: `
resource "omada_device_switch" "test" {
  ports = [{ port = 7, speed = 3 }]
}
`,
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`Invalid Configuration for Read-Only Attribute`),
		}},
	})
}
