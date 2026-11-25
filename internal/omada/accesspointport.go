// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package omada

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"unicode"
)

// Access point LAN ports (Devices -> an EAP -> Config): the Ethernet ports an
// EAP passes wired clients through, e.g. ETH2 on an EAP245.
//
// They live in the AP's own device document, as lanPortSettings, so the read
// and the write are the ones omada_access_point uses:
//
//	GET   /sites/{site}/eaps/{mac}   lanPortSettings[] in the device document
//	PATCH /sites/{site}/eaps/{mac}   {"lanPortSettings": [<one port>]}
//
// Read from a live 6.3.0.111 controller with two EAP245s: each port is named
// by lanPort ("ETH2") and carries no id. The uplink is not listed at all, and
// deviceMisc.lanVlanPorts names the VLAN-capable ports (["ETH2"]).
// localVlanId is absent until a VLAN has been set; a port set in the UI reads
// localVlanEnable true, localVlanId 20, custom true, with the network-id lists
// empty (the EAP245 has supportVlanTagged false).
//
// The write is inferred, not exercised on hardware: the body is the one
// MarkGodwin/tplink-omada-api sends, id set to the port's name, with its
// current poeOutEnable when the port reports one. That it merges into the one
// port named, rather than replacing the entry or the list, is also inferred,
// hence the read-back in UpdateAccessPointPort.
type AccessPointPort struct {
	LANPort string `json:"lanPort"`
	Name    string `json:"name"`

	// UplinkPort marks the port the AP itself is connected through: the
	// port's own uplinkPort or logicUplinkPort is present and not false (how
	// other clients and the Open API mark it; not seen live, as the EAP245
	// does not list its uplink), or deviceMisc.uplinkPort names the port. Its
	// VLAN is the AP's management traffic, so it is never written.
	UplinkPort bool `json:"-"`

	// SupportVLAN is the port's supportVlan flag, and true only when
	// deviceMisc could be read, its lanVlanPorts lists the port too, and its
	// supportUplinkPortConfig is false. An AP that does not send both has no
	// port this provider writes: without them, a listed uplink not flagged as
	// one could not be told apart.
	SupportVLAN bool `json:"supportVlan"`

	// LocalVLANEnable puts untagged traffic on the port into LocalVLANID.
	// Off, the port carries whatever the AP's uplink carries untagged.
	LocalVLANEnable bool `json:"localVlanEnable"`
	LocalVLANID     int  `json:"localVlanId"`
}

// AccessPointPortUpdate is the VLAN to set on one port.
type AccessPointPortUpdate struct {
	LANPort         string
	LocalVLANEnable bool
	LocalVLANID     int
}

// accessPointPorts is the part of the AP document this file reads.
type accessPointPorts struct {
	ports []AccessPointPort
	// raw holds the same entries undecoded, to check after a write that
	// nothing but the VLAN moved.
	raw []map[string]any
	// miscUnread is set when deviceMisc's uplinkPort is neither null nor a
	// string, or its lanVlanPorts is not a list of strings. Reads go on;
	// writes are refused, as the uplink check could not be made.
	miscUnread bool
}

func (c *Client) readAccessPointPorts(ctx context.Context, siteID, mac string) (*accessPointPorts, error) {
	var doc json.RawMessage
	if err := c.Do(ctx, http.MethodGet, accessPointPath(siteID, mac), nil, &doc); err != nil {
		return nil, fmt.Errorf("reading ports of access point %s: %w", mac, err)
	}
	var typed struct {
		LANPortSettings []AccessPointPort `json:"lanPortSettings"`
	}
	var raw struct {
		LANPortSettings []map[string]any `json:"lanPortSettings"`
		DeviceMisc      map[string]any   `json:"deviceMisc"`
	}
	for _, dst := range []any{&typed, &raw} {
		if err := json.Unmarshal(doc, dst); err != nil {
			return nil, fmt.Errorf("decoding ports of access point %s: %w", mac, err)
		}
	}
	out := &accessPointPorts{ports: typed.LANPortSettings, raw: raw.LANPortSettings}

	uplink := ""
	switch v := raw.DeviceMisc["uplinkPort"].(type) {
	case nil:
	case string:
		uplink = v // "ETH1(POE)" in the AP's own device profile; null live
	default:
		out.miscUnread = true
	}
	var vlanPorts []string
	switch v := raw.DeviceMisc["lanVlanPorts"].(type) {
	case nil:
	case []any:
		vlanPorts = []string{}
		for _, p := range v {
			s, ok := p.(string)
			if !ok {
				out.miscUnread = true
			}
			vlanPorts = append(vlanPorts, s)
		}
	default:
		out.miscUnread = true
	}
	// An AP whose uplink can be chosen may have it on a listed port that no
	// flag marks; false live on the EAP245.
	fixedUplink := raw.DeviceMisc["supportUplinkPortConfig"] == false
	for i := range out.ports {
		p := &out.ports[i]
		if namesPort(uplink, p.LANPort) {
			p.UplinkPort = true
		}
		for _, flag := range []string{"uplinkPort", "logicUplinkPort"} {
			if v, ok := out.raw[i][flag]; ok && v != false {
				p.UplinkPort = true
			}
		}
		if out.miscUnread || !fixedUplink || !slices.Contains(vlanPorts, p.LANPort) {
			p.SupportVLAN = false
		}
	}
	return out, nil
}

// namesPort reports whether s names the port: the port's name, in any case,
// alone or followed by anything but a letter or digit ("ETH1(POE)").
func namesPort(s, port string) bool {
	if len(s) < len(port) || !strings.EqualFold(s[:len(port)], port) {
		return false
	}
	if len(s) == len(port) {
		return true
	}
	r := rune(s[len(port)])
	return !unicode.IsLetter(r) && !unicode.IsDigit(r)
}

// ErrAccessPointPortNotFound is returned when the AP lists its ports and the
// one named is not among them, so callers can tell it apart from a failed
// request. An AP that lists no ports at all is not this: an AP that is
// disconnected or upgrading may report none for a while.
var ErrAccessPointPortNotFound = errors.New("access point port not found")

// ErrAccessPointNoPorts is returned when the AP lists no ports at all.
var ErrAccessPointNoPorts = errors.New("access point reports no LAN ports")

func (a *accessPointPorts) find(mac, port string) (*AccessPointPort, error) {
	names := make([]string, 0, len(a.ports))
	for i := range a.ports {
		if a.ports[i].LANPort == port {
			return &a.ports[i], nil
		}
		names = append(names, a.ports[i].LANPort)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("%w: access point %s", ErrAccessPointNoPorts, mac)
	}
	return nil, fmt.Errorf("%w: access point %s has no port %q (it has %s)",
		ErrAccessPointPortNotFound, mac, port, strings.Join(names, ", "))
}

// GetAccessPointPort returns one port by its name, e.g. "ETH2".
//
// A port the AP does not list is ErrAccessPointPortNotFound: the name is
// wrong, the MAC is another AP's, or the port is gone. An AP that lists no
// ports at all is ErrAccessPointNoPorts.
func (c *Client) GetAccessPointPort(ctx context.Context, siteID, mac, port string) (*AccessPointPort, error) {
	a, err := c.readAccessPointPorts(ctx, siteID, mac)
	if err != nil {
		return nil, err
	}
	return a.find(mac, port)
}

// portSettings are the keys of a lanPortSettings entry that are configuration
// rather than telemetry, as read live and as the Open API documents them.
// After a write, every one of them must be as it was on every port, apart
// from the VLAN of the port written.
var portSettings = []string{
	"name", "status", "poeOutEnable", "dot1xEnable", "mabEnable",
	"bandwidthControlEnable", "ingressRateLimit", "egressRateLimit",
	"tagged", "untagged", "taggedNetworkId", "untaggedNetworkId",
	"localVlanEnable", "localVlanId",
}

// UpdateAccessPointPort sets one port's VLAN and returns the port as read back.
//
// Refused before anything is sent: the AP's uplink (a wrong VLAN there cuts
// the AP off from the controller, and recovering it is a site visit), a port
// without VLAN support, enabling a VLAN without an id from 1 to 4094, and any
// write when deviceMisc's uplinkPort is neither null nor a string, or its
// lanVlanPorts is not a list of strings. Together these fail closed: a port is
// written only when the AP itself lists it as VLAN-capable (see SupportVLAN)
// and nothing marks it the uplink.
//
// The AP is read again after the write, because its document has already
// answered success for a value it ignored (see the channel note on
// AccessPoint). A VLAN that was not stored is an error, and so is any other
// setting that moved, on this port or another. That proves what the
// controller stored, not that the AP applied it.
//
// Writes to one AP's ports are serialised, from the first read to the
// read-back: Terraform applies resources in parallel, and another port's write
// in between would read as a setting that moved.
func (c *Client) UpdateAccessPointPort(ctx context.Context, siteID, mac string, p AccessPointPortUpdate) (*AccessPointPort, error) {
	lock, _ := c.apPortLocks.LoadOrStore(NormalizeMAC(mac), &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()

	before, err := c.readAccessPointPorts(ctx, siteID, mac)
	if err != nil {
		return nil, err
	}
	cur, err := before.find(mac, p.LANPort)
	if err != nil {
		return nil, err
	}
	if before.miscUnread {
		return nil, fmt.Errorf("refusing to write port %s of access point %s: its deviceMisc is not in the "+
			"shape this provider knows, so the AP's uplink cannot be told apart", p.LANPort, mac)
	}
	if cur.UplinkPort {
		return nil, fmt.Errorf("refusing to write port %s of access point %s: it is the AP's uplink, "+
			"and a wrong VLAN there cuts the AP off from the controller", p.LANPort, mac)
	}
	if !cur.SupportVLAN {
		return nil, fmt.Errorf("port %s of access point %s does not support a VLAN (the AP must list it "+
			"as VLAN-capable, in supportVlan and deviceMisc.lanVlanPorts, and have a fixed uplink, "+
			"deviceMisc.supportUplinkPortConfig false)", p.LANPort, mac)
	}
	if p.LocalVLANEnable && (p.LocalVLANID < 1 || p.LocalVLANID > 4094) {
		return nil, fmt.Errorf("port %s of access point %s: a VLAN id from 1 to 4094 is needed to enable its VLAN, got %d",
			p.LANPort, mac, p.LocalVLANID)
	}

	port := map[string]any{
		"id":              p.LANPort,
		"lanPort":         p.LANPort,
		"localVlanEnable": p.LocalVLANEnable,
		"localVlanId":     p.LocalVLANID,
	}
	// PoE goes back as read, and only if the port reports it.
	for _, e := range before.raw {
		if v, ok := e["poeOutEnable"]; ok && e["lanPort"] == p.LANPort {
			port["poeOutEnable"] = v
		}
	}
	body := map[string]any{"lanPortSettings": []any{port}}
	if err := c.Do(ctx, http.MethodPatch, accessPointPath(siteID, mac), body, nil); err != nil {
		return nil, fmt.Errorf("updating port %s of access point %s: %w", p.LANPort, mac, err)
	}

	after, err := c.readAccessPointPorts(ctx, siteID, mac)
	if err != nil {
		return nil, fmt.Errorf("reading port %s of access point %s after writing it: %w", p.LANPort, mac, err)
	}
	got, err := after.find(mac, p.LANPort)
	if err != nil {
		return nil, fmt.Errorf("reading port %s of access point %s after writing it: %w", p.LANPort, mac, err)
	}
	// While the VLAN is off, its id has no effect and the controller may keep
	// or drop it, so only an enabled VLAN's id is checked.
	if got.LocalVLANEnable != p.LocalVLANEnable || (p.LocalVLANEnable && got.LocalVLANID != p.LocalVLANID) {
		return nil, fmt.Errorf("access point %s reported success but port %s has VLAN enabled=%t id=%d, not enabled=%t id=%d",
			mac, p.LANPort, got.LocalVLANEnable, got.LocalVLANID, p.LocalVLANEnable, p.LocalVLANID)
	}
	if moved := movedSettings(before.raw, after.raw, p.LANPort); len(moved) > 0 {
		return nil, fmt.Errorf("writing the VLAN of port %s on access point %s also changed %s",
			p.LANPort, mac, strings.Join(moved, ", "))
	}
	return got, nil
}

// movedSettings lists the settings that differ between two reads of the
// ports, as "<port>.<key>", leaving out the written port's VLAN.
func movedSettings(before, after []map[string]any, written string) []string {
	byName := func(entries []map[string]any) map[string]map[string]any {
		out := map[string]map[string]any{}
		for _, e := range entries {
			name, _ := e["lanPort"].(string)
			out[name] = e
		}
		return out
	}
	b, a := byName(before), byName(after)
	var moved []string
	for name, old := range b {
		cur, ok := a[name]
		if !ok {
			moved = append(moved, name+" (gone)")
			continue
		}
		for _, k := range portSettings {
			if name == written && (k == "localVlanEnable" || k == "localVlanId") {
				continue
			}
			if !reflect.DeepEqual(old[k], cur[k]) {
				moved = append(moved, name+"."+k)
			}
		}
	}
	if len(after) != len(before) {
		moved = append(moved, fmt.Sprintf("the number of ports (%d -> %d)", len(before), len(after)))
	}
	for name := range a {
		if _, ok := b[name]; !ok {
			moved = append(moved, name+" (new)")
		}
	}
	slices.Sort(moved)
	return moved
}
