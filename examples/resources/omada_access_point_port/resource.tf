# Put whatever is plugged into the AP's ETH2 (a TV, a printer) on the IoT
# network, untagged. The AP's uplink is never written.
#
# vlan_id, if set, needs vlan_enable = true; left out, it keeps the port's id.
resource "omada_access_point_port" "tv" {
  ap_mac = data.omada_devices.all.devices[0].mac
  port   = "ETH2"

  vlan_enable = true
  vlan_id     = omada_network.iot.vlan_id
}
