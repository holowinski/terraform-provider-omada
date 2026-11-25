# A reusable port group, referenced from a firewall ACL via source/destination
# type 2.
resource "omada_port_group" "web" {
  name  = "web-ports"
  ports = ["80", "443", "8080-8090"]
}

# An IP-Port Group: the ports on these addresses only. A firewall ACL with it
# as destination matches one host on one port.
resource "omada_port_group" "web_server" {
  name    = "web-server-https"
  ip_list = [{ ip = "10.10.10.5", mask = 32, description = "web server" }]
  ports   = ["443"]
}
