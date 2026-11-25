// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// TestAccPortGroupResource drives create → import → update of omada_port_group
// against the in-test mock controller, adding ip_list and dropping it again.
// Requires TF_ACC=1.
func TestAccPortGroupResource(t *testing.T) {
	srv := newMockController(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testProviderConfig(srv.URL) + `
resource "omada_port_group" "test" {
  name  = "web-ports"
  ports = ["8080", "9000-9100"]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("omada_port_group.test", "id"),
					resource.TestCheckResourceAttr("omada_port_group.test", "port_type", "0"),
					resource.TestCheckResourceAttr("omada_port_group.test", "ports.#", "2"),
					resource.TestCheckResourceAttr("omada_port_group.test", "ports.0", "8080"),
					resource.TestCheckResourceAttr("omada_port_group.test", "ports.1", "9000-9100"),
					resource.TestCheckResourceAttr("omada_port_group.test", "site_id", "site-1"),
					resource.TestCheckResourceAttr("omada_port_group.test", "ip_list.#", "0"),
				),
			},
			{ResourceName: "omada_port_group.test", ImportState: true, ImportStateVerify: true},
			{
				Config: testProviderConfig(srv.URL) + `
resource "omada_port_group" "test" {
  name    = "web-ports"
  ip_list = [{ ip = "10.10.10.5", mask = 32, description = "web server" }]
  ports   = ["443"]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omada_port_group.test", "ports.#", "1"),
					resource.TestCheckResourceAttr("omada_port_group.test", "ports.0", "443"),
					resource.TestCheckResourceAttr("omada_port_group.test", "ip_list.#", "1"),
					resource.TestCheckResourceAttr("omada_port_group.test", "ip_list.0.ip", "10.10.10.5"),
					resource.TestCheckResourceAttr("omada_port_group.test", "ip_list.0.mask", "32"),
					resource.TestCheckResourceAttr("omada_port_group.test", "ip_list.0.description", "web server"),
				),
			},
			{ResourceName: "omada_port_group.test", ImportState: true, ImportStateVerify: true},
			{
				Config: testProviderConfig(srv.URL) + `
resource "omada_port_group" "test" {
  name  = "web-ports"
  ports = ["443"]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omada_port_group.test", "ip_list.#", "0"),
				),
			},
		},
	})
}
