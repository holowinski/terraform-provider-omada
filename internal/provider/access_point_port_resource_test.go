// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"fmt"
	"net/http"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// apPortsDebug sends a request to one of the mock's debug controls.
func apPortsDebug(t *testing.T, base, method, path string) func() {
	return func() {
		req, _ := http.NewRequest(method, base+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("debug %s %s: %v", method, path, err)
		}
		_ = resp.Body.Close()
	}
}

// TestAccAccessPointPortResource drives adopt → update → import → drift →
// update of an EAP245's ETH2, and checks what was sent as well as what was
// stored: an unchanged port is never written, and nothing else on the port
// moves.
func TestAccAccessPointPortResource(t *testing.T) {
	srv := newMockController(t)

	writes := func(want int) resource.TestCheckFunc {
		return func(*terraform.State) error {
			got, _ := rawStore(t, srv.URL, "accessPointPorts")["portWrites"]["ETH2"].(float64)
			if int(got) != want {
				return fmt.Errorf("expected %d write(s) of ETH2 so far, got %v", want, got)
			}
			return nil
		}
	}
	config := func(attrs string) string {
		return testProviderConfig(srv.URL) + `
resource "omada_access_point_port" "tv" {
  ap_mac = "ac:84:c6:00:00:01"
  port   = "ETH2"
` + attrs + `
}`
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// Adopting the port with nothing configured reads it and writes
				// nothing. localVlanId is absent until a VLAN is set: 0.
				Config: config(""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omada_access_point_port.tv", "id", "AC-84-C6-00-00-01/ETH2"),
					resource.TestCheckResourceAttr("omada_access_point_port.tv", "name", "ETH2"),
					resource.TestCheckResourceAttr("omada_access_point_port.tv", "uplink", "false"),
					resource.TestCheckResourceAttr("omada_access_point_port.tv", "support_vlan", "true"),
					resource.TestCheckResourceAttr("omada_access_point_port.tv", "vlan_enable", "false"),
					resource.TestCheckResourceAttr("omada_access_point_port.tv", "vlan_id", "0"),
					writes(0),
				),
			},
			{
				// Enabling the VLAN of a port that never had one needs an id;
				// refused before anything is sent.
				Config:      config(`vlan_enable = true`),
				ExpectError: regexp.MustCompile(`a\s+VLAN\s+id\s+from\s+1\s+to\s+4094\s+is\s+needed`),
			},
			{
				Config:      config("vlan_enable = true\nvlan_id = 4095"),
				ExpectError: regexp.MustCompile(`(?s)vlan_id.*between\s+1\s+and\s+4094`),
			},
			{
				// An id without the VLAN on is refused at plan, whether
				// vlan_enable is false or left out.
				Config:      config("vlan_enable = false\nvlan_id = 20"),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`vlan_id\s+requires\s+vlan_enable\s+=\s+true`),
			},
			{
				Config:      config("vlan_id = 20"),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`vlan_id\s+requires\s+vlan_enable\s+=\s+true`),
			},
			{
				Config: config("vlan_enable = true\nvlan_id = 20"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omada_access_point_port.tv", "vlan_enable", "true"),
					resource.TestCheckResourceAttr("omada_access_point_port.tv", "vlan_id", "20"),
					writes(1),
				),
			},
			{
				ResourceName:      "omada_access_point_port.tv",
				ImportState:       true,
				ImportStateId:     "AC-84-C6-00-00-01/ETH2",
				ImportStateVerify: true,
				// State keeps the config's colon spelling; import reads the
				// controller's dashes, as for omada_access_point.
				ImportStateVerifyIgnore: []string{"ap_mac"},
			},
			{
				ResourceName:            "omada_access_point_port.tv",
				ImportState:             true,
				ImportStateId:           "Default/AC-84-C6-00-00-01/ETH2",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"ap_mac"},
			},
			{
				ResourceName:  "omada_access_point_port.tv",
				ImportState:   true,
				ImportStateId: "AC-84-C6-00-00-01",
				ExpectError:   regexp.MustCompile(`Unexpected import id`),
			},
			{
				ResourceName:  "omada_access_point_port.tv",
				ImportState:   true,
				ImportStateId: "/ETH2",
				ExpectError:   regexp.MustCompile(`Unexpected import id`),
			},
			{
				// A port name the AP does not have says which ones it has.
				ResourceName:  "omada_access_point_port.tv",
				ImportState:   true,
				ImportStateId: "AC-84-C6-00-00-01/eth2",
				ExpectError:   regexp.MustCompile(`(?s)has\s+no\s+port\s+"eth2".*it\s+has\s+ETH2`),
			},
			{
				// A VLAN changed in the UI shows as drift, as the UI stores it.
				PreConfig:          apPortsDebug(t, srv.URL, http.MethodPost, "/debug/accessPointPorts?ui_vlan=30"),
				Config:             config("vlan_enable = true\nvlan_id = 20"),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// Applying puts it back, with one more write.
				Config: config("vlan_enable = true\nvlan_id = 20"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("omada_access_point_port.tv", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omada_access_point_port.tv", "vlan_id", "20"),
					writes(2),
				),
			},
			{
				// vlan_id left out keeps the device's, so nothing differs:
				// Update runs, as vlan_id is unknown in the plan, but sends
				// nothing.
				Config: config(`vlan_enable = true`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omada_access_point_port.tv", "vlan_id", "20"),
					writes(2),
				),
			},
			{
				// Back to the uplink's untagged network, vlan_id left out.
				Config: config(`vlan_enable = false`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omada_access_point_port.tv", "vlan_enable", "false"),
					writes(3),
				),
			},
		},
		// Destroying only forgets the port.
		CheckDestroy: writes(3),
	})
}

// TestAccAccessPointPortResourceUnknownEnable covers vlan_enable known only at
// apply: the check is skipped while it is unknown, and ValidateConfig, which
// Terraform runs again with the values known, refuses an id with the VLAN off
// before anything is sent.
func TestAccAccessPointPortResourceUnknownEnable(t *testing.T) {
	srv := newMockController(t)
	config := func(enable bool) string {
		return testProviderConfig(srv.URL) + fmt.Sprintf(`
resource "terraform_data" "enable" {
  input = %t
}

resource "omada_access_point_port" "tv" {
  ap_mac      = "AC-84-C6-00-00-01"
  port        = "ETH2"
  vlan_enable = terraform_data.enable.output
  vlan_id     = 20
}`, enable)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      config(false),
				ExpectError: regexp.MustCompile(`vlan_id\s+requires\s+vlan_enable\s+=\s+true`),
			},
			{
				PreConfig: func() {
					if got, _ := rawStore(t, srv.URL, "accessPointPorts")["requests"]["patch"].(float64); got != 0 {
						t.Fatalf("the refused apply patched the AP %v time(s)", got)
					}
				},
				Config: config(true),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omada_access_point_port.tv", "vlan_enable", "true"),
					resource.TestCheckResourceAttr("omada_access_point_port.tv", "vlan_id", "20"),
				),
			},
		},
	})
}

// TestAccAccessPointPortResourceReadErrors keeps the port in state when the
// AP's document fails or lists no ports, and drops it once the AP is gone.
func TestAccAccessPointPortResourceReadErrors(t *testing.T) {
	srv := newMockController(t)
	config := testProviderConfig(srv.URL) + `
resource "omada_access_point_port" "tv" {
  ap_mac = "AC-84-C6-00-00-01"
  port   = "ETH2"
}`

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig:   apPortsDebug(t, srv.URL, http.MethodPost, "/debug/accessPointPorts?down=true"),
				Config:      config,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`Unable\s+to\s+read\s+access\s+point\s+port`),
			},
			{
				// The whole controller failing, device list too, still keeps it.
				PreConfig:   apPortsDebug(t, srv.URL, http.MethodPost, "/debug/devices?down=true"),
				Config:      config,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`Unable\s+to\s+read\s+access\s+point\s+port`),
			},
			{
				// An AP listing no ports, as one disconnected or upgrading
				// may, keeps the port as last read: no error, no change.
				PreConfig: func() {
					apPortsDebug(t, srv.URL, http.MethodPost, "/debug/devices?down=false")()
					apPortsDebug(t, srv.URL, http.MethodPost, "/debug/accessPointPorts?down=false&no_ports=true")()
				},
				Config:   config,
				PlanOnly: true,
			},
			{
				// Still no ports, and the device list failing too: kept.
				PreConfig: apPortsDebug(t, srv.URL, http.MethodPost, "/debug/devices?down=true"),
				Config:    config,
				PlanOnly:  true,
			},
			{
				PreConfig: func() {
					apPortsDebug(t, srv.URL, http.MethodPost, "/debug/devices?down=false")()
					apPortsDebug(t, srv.URL, http.MethodDelete, "/debug/devices?mac=AC-84-C6-00-00-01")()
				},
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

// TestAccAccessPointPortResourceNoPorts covers an AP that lists no ports: an
// import then fails rather than storing an empty port, and once the AP has
// also left the site, the port is dropped.
func TestAccAccessPointPortResourceNoPorts(t *testing.T) {
	srv := newMockController(t)
	config := testProviderConfig(srv.URL) + `
resource "omada_access_point_port" "tv" {
  ap_mac = "AC-84-C6-00-00-01"
  port   = "ETH2"
}`

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig:     apPortsDebug(t, srv.URL, http.MethodPost, "/debug/accessPointPorts?no_ports=true"),
				ResourceName:  "omada_access_point_port.tv",
				ImportState:   true,
				ImportStateId: "AC-84-C6-00-00-01/ETH2",
				ExpectError:   regexp.MustCompile(`reports\s+no\s+LAN\s+ports`),
			},
			{
				PreConfig: func() {
					apPortsDebug(t, srv.URL, http.MethodPost, "/debug/accessPointPorts?serve_unadopted=true")()
					apPortsDebug(t, srv.URL, http.MethodDelete, "/debug/devices?mac=AC-84-C6-00-00-01")()
				},
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

// TestAccAccessPointPortResourcePortGone reads deviceMisc into uplink and
// support_vlan, and drops the port from state once the AP, still adopted, no
// longer lists it.
func TestAccAccessPointPortResourcePortGone(t *testing.T) {
	srv := newMockController(t)
	config := testProviderConfig(srv.URL) + `
resource "omada_access_point_port" "tv" {
  ap_mac = "AC-84-C6-00-00-01"
  port   = "ETH2"
}`

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omada_access_point_port.tv", "uplink", "false"),
					resource.TestCheckResourceAttr("omada_access_point_port.tv", "support_vlan", "true"),
				),
			},
			{
				PreConfig:    apPortsDebug(t, srv.URL, http.MethodPost, "/debug/accessPointPorts?misc=uplink"),
				RefreshState: true,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omada_access_point_port.tv", "uplink", "true"),
					resource.TestCheckResourceAttr("omada_access_point_port.tv", "support_vlan", "false"),
				),
			},
			{
				PreConfig:          apPortsDebug(t, srv.URL, http.MethodPost, "/debug/accessPointPorts?drop_port=true"),
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

// A port the AP does not have is an error naming the ports it does have, with
// nothing sent. The uplink and no-VLAN refusals are in the client's tests: an
// EAP245 lists neither kind of port.
func TestAccAccessPointPortResourceUnknownPort(t *testing.T) {
	srv := newMockController(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testProviderConfig(srv.URL) + `
resource "omada_access_point_port" "this" {
  ap_mac      = "AC-84-C6-00-00-01"
  port        = "ETH1"
  vlan_enable = true
  vlan_id     = 20
}`,
				ExpectError: regexp.MustCompile(`(?s)has\s+no\s+port\s+"ETH1".*it\s+has\s+ETH2`),
			},
		},
		CheckDestroy: func(*terraform.State) error {
			if got, _ := rawStore(t, srv.URL, "accessPointPorts")["requests"]["patch"].(float64); got != 0 {
				return fmt.Errorf("the AP was patched %v time(s)", got)
			}
			return nil
		},
	})
}
