# By "<ap mac>/<port>" (the primary site is assumed):
terraform import omada_access_point_port.tv 10-5A-95-89-C3-64/ETH2

# Or "<site>/<ap mac>/<port>" to name the site explicitly:
terraform import omada_access_point_port.tv Home/10-5A-95-89-C3-64/ETH2
