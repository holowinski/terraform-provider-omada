// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package omada

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// apPortController serves one AP. ETH2 is an EAP245's port as a live 6.3
// controller reports it (no id, no localVlanId yet), with both uplink flags
// false as the Open API reports them. ETH0 is marked uplink, ETH5 the logical
// uplink, ETH6 and ETH7 with an uplink flag that is not a bool or is null, and
// ETH3 lacks VLAN support, as other models and clients report them; all are in
// deviceMisc.lanVlanPorts, so only their own flags refuse them.
//
// onPatch decides what a PATCH does to a stored port; nil ignores the write
// while answering success, as the AP document does for a radio's channel.
type apPortController struct {
	ports   []map[string]any
	misc    map[string]any
	patches []map[string]any
	onPatch func(stored, sent map[string]any, all []map[string]any)
	// addAfterPatch, if set, is a port that appears once a PATCH arrives;
	// dropAfterPatch names one that disappears.
	addAfterPatch  map[string]any
	dropAfterPatch string
	// delay holds each request before it is served, so concurrent writes
	// interleave; a read right after a write is held three times as long, so
	// another write lands inside the read-back window too.
	delay     time.Duration
	afterSent bool
	mu        sync.Mutex
}

func newAPPortController(t *testing.T, onPatch func(stored, sent map[string]any, all []map[string]any)) (*Client, *apPortController) {
	t.Helper()
	ctl := &apPortController{
		ports: []map[string]any{
			{"lanPort": "ETH0", "uplinkPort": true, "supportVlan": true, "localVlanEnable": false, "status": true},
			{"lanPort": "ETH2", "name": "ETH2", "uplinkPort": false, "logicUplinkPort": false, "supportVlan": true, "localVlanEnable": false,
				"supportPoe": true, "poeOutEnable": true, "status": true, "dot1xEnable": false},
			{"lanPort": "ETH3", "supportVlan": false, "localVlanEnable": false, "status": true},
			{"lanPort": "ETH5", "logicUplinkPort": true, "uplinkPort": false, "supportVlan": true, "localVlanEnable": false, "status": true},
			{"lanPort": "ETH6", "uplinkPort": "yes", "supportVlan": true, "localVlanEnable": false, "status": true},
			{"lanPort": "ETH7", "uplinkPort": nil, "supportVlan": true, "localVlanEnable": false, "status": true},
		},
		misc: map[string]any{"uplinkPort": nil, "supportUplinkPortConfig": false,
			"lanVlanPorts": []any{"ETH0", "ETH2", "ETH3", "ETH5", "ETH6", "ETH7"}},
		onPatch: onPatch,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 0, "", map[string]any{"omadacId": "abc123", "controllerVer": "6.3.0.111", "apiVer": "3", "type": 1})
	})
	mux.HandleFunc("/abc123/api/v2/login", func(w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 0, "", map[string]any{"token": "tok-xyz"})
	})
	mux.HandleFunc("/abc123/api/v2/sites/site-1/eaps/AA-BB-CC-DD-EE-FF", func(w http.ResponseWriter, r *http.Request) {
		ctl.mu.Lock()
		wait := ctl.delay
		if r.Method == http.MethodGet && ctl.afterSent {
			wait *= 3
		}
		ctl.afterSent = r.Method == http.MethodPatch
		ctl.mu.Unlock()
		time.Sleep(wait)
		ctl.mu.Lock()
		defer ctl.mu.Unlock()
		if r.Method == http.MethodPatch {
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			ctl.patches = append(ctl.patches, in)
			if ctl.onPatch != nil {
				for _, s := range in["lanPortSettings"].([]any) {
					sent := s.(map[string]any)
					for _, p := range ctl.ports {
						if p["lanPort"] == sent["lanPort"] {
							ctl.onPatch(p, sent, ctl.ports)
						}
					}
				}
			}
			if ctl.addAfterPatch != nil {
				ctl.ports = append(ctl.ports, ctl.addAfterPatch)
				ctl.addAfterPatch = nil
			}
			if ctl.dropAfterPatch != "" {
				ctl.ports = slices.DeleteFunc(ctl.ports, func(p map[string]any) bool { return p["lanPort"] == ctl.dropAfterPatch })
				ctl.dropAfterPatch = ""
			}
		}
		writeEnvelope(w, 0, "", map[string]any{
			"mac": "AA-BB-CC-DD-EE-FF", "lanPortSettings": ctl.ports, "deviceMisc": ctl.misc,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := NewClient(context.Background(), srv.URL, "admin", "secret", true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c, ctl
}

func merge(stored, sent map[string]any, _ []map[string]any) {
	for k, v := range sent {
		if k != "id" {
			stored[k] = v
		}
	}
}

func setVLAN20(c *Client, port string) (*AccessPointPort, error) {
	return c.UpdateAccessPointPort(context.Background(), "site-1", "AA-BB-CC-DD-EE-FF", AccessPointPortUpdate{
		LANPort: port, LocalVLANEnable: true, LocalVLANID: 20,
	})
}

func TestUpdateAccessPointPortBody(t *testing.T) {
	c, ctl := newAPPortController(t, merge)

	got, err := setVLAN20(c, "ETH2")
	if err != nil {
		t.Fatalf("UpdateAccessPointPort: %v", err)
	}
	if !got.LocalVLANEnable || got.LocalVLANID != 20 {
		t.Fatalf("returned port should be the read-back, got %+v", got)
	}
	if len(ctl.patches) != 1 || len(ctl.patches[0]) != 1 {
		t.Fatalf("expected one PATCH carrying lanPortSettings alone, got %v", ctl.patches)
	}
	sent := ctl.patches[0]["lanPortSettings"].([]any)
	if len(sent) != 1 {
		t.Fatalf("the PATCH should carry the one port being changed, got %v", sent)
	}
	want := map[string]any{
		"id": "ETH2", "lanPort": "ETH2", "localVlanEnable": true, "localVlanId": float64(20),
		// Sent back as read, so a controller that replaced the whole entry
		// would not switch PoE off.
		"poeOutEnable": true,
	}
	port := sent[0].(map[string]any)
	if len(port) != len(want) {
		t.Fatalf("port body: got %v, want %v", port, want)
	}
	for k, v := range want {
		if port[k] != v {
			t.Fatalf("port body %s: got %v, want %v", k, port[k], v)
		}
	}
}

// A PATCH the controller answers with success but does not apply must be an
// error: otherwise the apply looks clean and the next plan shows the same
// change again, forever.
func TestUpdateAccessPointPortChecksTheVLAN(t *testing.T) {
	for _, tc := range []struct {
		name    string
		start   bool
		p       AccessPointPortUpdate
		onPatch func(stored, sent map[string]any, all []map[string]any)
	}{
		{"whole write ignored", false, AccessPointPortUpdate{LANPort: "ETH2", LocalVLANEnable: true, LocalVLANID: 20}, nil},
		{"id not stored", false, AccessPointPortUpdate{LANPort: "ETH2", LocalVLANEnable: true, LocalVLANID: 20},
			func(stored, sent map[string]any, all []map[string]any) {
				merge(stored, sent, all)
				stored["localVlanId"] = 1
			}},
		{"disable ignored", true, AccessPointPortUpdate{LANPort: "ETH2", LocalVLANEnable: false, LocalVLANID: 20},
			func(stored, sent map[string]any, _ []map[string]any) {
				stored["localVlanId"] = sent["localVlanId"]
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, ctl := newAPPortController(t, tc.onPatch)
			if tc.start {
				ctl.ports[1]["localVlanEnable"], ctl.ports[1]["localVlanId"] = true, 20
			}
			_, err := c.UpdateAccessPointPort(context.Background(), "site-1", "AA-BB-CC-DD-EE-FF", tc.p)
			if err == nil || !strings.Contains(err.Error(), "reported success") {
				t.Fatalf("expected an error naming the ignored write, got %v", err)
			}
		})
	}
}

// The port missing from the read-back is an error, not a crash.
func TestUpdateAccessPointPortPortGoneAfterWrite(t *testing.T) {
	c, ctl := newAPPortController(t, func(_, _ map[string]any, _ []map[string]any) {})
	ctl.dropAfterPatch = "ETH2"

	if _, err := setVLAN20(c, "ETH2"); err == nil || !strings.Contains(err.Error(), "after writing it") {
		t.Fatalf("expected an error about the read-back, got %v", err)
	}
}

// While the VLAN is off its id has no effect, so a controller that drops the
// id on disable is not an error.
func TestUpdateAccessPointPortDisableIgnoresID(t *testing.T) {
	c, ctl := newAPPortController(t, func(stored, sent map[string]any, all []map[string]any) {
		merge(stored, sent, all)
		if sent["localVlanEnable"] == false {
			delete(stored, "localVlanId")
		}
	})
	eth2 := ctl.ports[1]
	eth2["localVlanEnable"], eth2["localVlanId"] = true, 20

	_, err := c.UpdateAccessPointPort(context.Background(), "site-1", "AA-BB-CC-DD-EE-FF", AccessPointPortUpdate{
		LANPort: "ETH2", LocalVLANEnable: false, LocalVLANID: 20,
	})
	if err != nil {
		t.Fatalf("disabling the VLAN should succeed, got %v", err)
	}
}

// A controller that took the entry, or the list, as a replacement would reset
// what was not sent. Any setting that moves, on this port or another, is an
// error naming it.
func TestUpdateAccessPointPortChecksNothingElseMoved(t *testing.T) {
	for _, tc := range []struct {
		name string
		also func(stored map[string]any, all []map[string]any)
		want string
	}{
		{"PoE", func(s map[string]any, _ []map[string]any) { s["poeOutEnable"] = false }, "ETH2.poeOutEnable"},
		{"status", func(s map[string]any, _ []map[string]any) { s["status"] = false }, "ETH2.status"},
		{"a dropped key", func(s map[string]any, _ []map[string]any) { delete(s, "dot1xEnable") }, "ETH2.dot1xEnable"},
		{"another port", func(_ map[string]any, all []map[string]any) { all[2]["localVlanEnable"] = true }, "ETH3.localVlanEnable"},
		{"a port added", nil, "ETH4 (new)"},
		{"a port gone", nil, "ETH3 (gone)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, ctl := newAPPortController(t, func(stored, sent map[string]any, all []map[string]any) {
				merge(stored, sent, all)
				if tc.also != nil {
					tc.also(stored, all)
				}
			})
			switch tc.name {
			case "a port added":
				ctl.addAfterPatch = map[string]any{"lanPort": "ETH4", "supportVlan": true, "status": true}
			case "a port gone":
				ctl.dropAfterPatch = "ETH3"
			}
			if _, err := setVLAN20(c, "ETH2"); err == nil || !strings.Contains(err.Error(), "also changed "+tc.want) {
				t.Fatalf("expected an error naming %s, got %v", tc.want, err)
			}
		})
	}
}

// Every configuration key of a port is compared after the write.
func TestUpdateAccessPointPortChecksEverySetting(t *testing.T) {
	for _, key := range portSettings {
		t.Run(key, func(t *testing.T) {
			c, _ := newAPPortController(t, func(stored, sent map[string]any, all []map[string]any) {
				merge(stored, sent, all)
				var moved any = "moved"
				switch key {
				case "localVlanEnable":
					moved = true
				case "localVlanId":
					moved = 99
				}
				all[2][key] = moved
			})
			if _, err := setVLAN20(c, "ETH2"); err == nil || !strings.Contains(err.Error(), "also changed ETH3."+key) {
				t.Fatalf("expected an error naming ETH3.%s, got %v", key, err)
			}
		})
	}
}

// A port that reports no poeOutEnable is not sent one.
func TestUpdateAccessPointPortNoPoEKey(t *testing.T) {
	c, ctl := newAPPortController(t, merge)
	ctl.ports[2]["supportVlan"] = true

	if _, err := setVLAN20(c, "ETH3"); err != nil {
		t.Fatalf("UpdateAccessPointPort: %v", err)
	}
	port := ctl.patches[0]["lanPortSettings"].([]any)[0].(map[string]any)
	if _, ok := port["poeOutEnable"]; ok {
		t.Fatalf("poeOutEnable sent for a port without one: %v", port)
	}
}

// Two ports of one AP written at once, as Terraform's parallel apply does,
// each resource spelling the MAC its own way: neither sees the other's write
// as a setting that moved.
func TestUpdateAccessPointPortSerialisesPerAP(t *testing.T) {
	c, ctl := newAPPortController(t, merge)
	ctl.ports[2]["supportVlan"] = true
	ctl.delay = 20 * time.Millisecond

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, w := range []struct{ mac, port string }{{"AA-BB-CC-DD-EE-FF", "ETH2"}, {"aa:bb:cc:dd:ee:ff", "ETH3"}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = c.UpdateAccessPointPort(context.Background(), "site-1", w.mac, AccessPointPortUpdate{
				LANPort: w.port, LocalVLANEnable: true, LocalVLANID: 20,
			})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
}

func TestUpdateAccessPointPortRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		misc map[string]any
		p    AccessPointPortUpdate
		want string
	}{
		{"marked uplink", nil, AccessPointPortUpdate{LANPort: "ETH0", LocalVLANEnable: true, LocalVLANID: 20}, "it is the AP's uplink"},
		{"marked logical uplink", nil, AccessPointPortUpdate{LANPort: "ETH5", LocalVLANEnable: true, LocalVLANID: 20}, "it is the AP's uplink"},
		{"uplink flag in an unknown shape", nil, AccessPointPortUpdate{LANPort: "ETH6", LocalVLANEnable: true, LocalVLANID: 20}, "it is the AP's uplink"},
		{"uplink flag null", nil, AccessPointPortUpdate{LANPort: "ETH7", LocalVLANEnable: true, LocalVLANID: 20}, "it is the AP's uplink"},
		{"named uplink in deviceMisc", map[string]any{"uplinkPort": "ETH2", "lanVlanPorts": []any{"ETH2"}},
			AccessPointPortUpdate{LANPort: "ETH2", LocalVLANEnable: true, LocalVLANID: 20}, "it is the AP's uplink"},
		{"named uplink in deviceMisc, with a spaced suffix", map[string]any{"uplinkPort": "ETH2 (PoE)", "lanVlanPorts": []any{"ETH2"}},
			AccessPointPortUpdate{LANPort: "ETH2", LocalVLANEnable: true, LocalVLANID: 20}, "it is the AP's uplink"},
		{"named uplink in deviceMisc, in another case", map[string]any{"uplinkPort": "eth2-poe", "lanVlanPorts": []any{"ETH2"}},
			AccessPointPortUpdate{LANPort: "ETH2", LocalVLANEnable: true, LocalVLANID: 20}, "it is the AP's uplink"},
		{"named uplink in deviceMisc, with its PoE", map[string]any{"uplinkPort": "ETH2(POE)", "lanVlanPorts": []any{"ETH2"}},
			AccessPointPortUpdate{LANPort: "ETH2", LocalVLANEnable: true, LocalVLANID: 20}, "it is the AP's uplink"},
		{"no VLAN support", nil, AccessPointPortUpdate{LANPort: "ETH3", LocalVLANEnable: true, LocalVLANID: 20}, "does not support a VLAN"},
		{"not in lanVlanPorts", map[string]any{"lanVlanPorts": []any{"ETH3"}},
			AccessPointPortUpdate{LANPort: "ETH2", LocalVLANEnable: true, LocalVLANID: 20}, "does not support a VLAN"},
		{"no lanVlanPorts", map[string]any{"uplinkPort": nil},
			AccessPointPortUpdate{LANPort: "ETH2", LocalVLANEnable: true, LocalVLANID: 20}, "does not support a VLAN"},
		{"uplink port configurable", map[string]any{"supportUplinkPortConfig": true, "lanVlanPorts": []any{"ETH2"}},
			AccessPointPortUpdate{LANPort: "ETH2", LocalVLANEnable: true, LocalVLANID: 20}, "does not support a VLAN"},
		{"uplink port configurability not reported", map[string]any{"lanVlanPorts": []any{"ETH2"}},
			AccessPointPortUpdate{LANPort: "ETH2", LocalVLANEnable: true, LocalVLANID: 20}, "does not support a VLAN"},
		{"no deviceMisc", map[string]any{},
			AccessPointPortUpdate{LANPort: "ETH2", LocalVLANEnable: true, LocalVLANID: 20}, "does not support a VLAN"},
		{"a lanVlanPorts entry in an unknown shape", map[string]any{"lanVlanPorts": []any{"ETH2", 1}},
			AccessPointPortUpdate{LANPort: "ETH2", LocalVLANEnable: true, LocalVLANID: 20}, "deviceMisc is not in the shape"},
		{"deviceMisc in an unknown shape", map[string]any{"uplinkPort": map[string]any{"port": "ETH1"}},
			AccessPointPortUpdate{LANPort: "ETH2", LocalVLANEnable: true, LocalVLANID: 20}, "deviceMisc is not in the shape"},
		{"lanVlanPorts in an unknown shape", map[string]any{"lanVlanPorts": "ETH2"},
			AccessPointPortUpdate{LANPort: "ETH2", LocalVLANEnable: true, LocalVLANID: 20}, "deviceMisc is not in the shape"},
		{"no VLAN id", nil, AccessPointPortUpdate{LANPort: "ETH2", LocalVLANEnable: true}, "a VLAN id from 1 to 4094 is needed"},
		{"VLAN id too high", nil, AccessPointPortUpdate{LANPort: "ETH2", LocalVLANEnable: true, LocalVLANID: 4095}, "a VLAN id from 1 to 4094 is needed"},
		{"no such port", nil, AccessPointPortUpdate{LANPort: "ETH9", LocalVLANEnable: true, LocalVLANID: 20}, `no port "ETH9" (it has ETH0, ETH2, ETH3, ETH5, ETH6, ETH7)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, ctl := newAPPortController(t, merge)
			if tc.misc != nil {
				ctl.misc = tc.misc
			}
			_, err := c.UpdateAccessPointPort(context.Background(), "site-1", "AA-BB-CC-DD-EE-FF", tc.p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
			if len(ctl.patches) != 0 {
				t.Fatalf("nothing should have been sent, got %v", ctl.patches)
			}
		})
	}
}

// deviceMisc is folded into what a read reports, so state shows what a write
// would refuse; an unknown shape there never fails a read.
func TestGetAccessPointPortFoldsDeviceMisc(t *testing.T) {
	c, ctl := newAPPortController(t, merge)
	ctl.misc = map[string]any{"uplinkPort": "ETH2(POE)", "lanVlanPorts": []any{"ETH0"}}

	p, err := c.GetAccessPointPort(context.Background(), "site-1", "AA-BB-CC-DD-EE-FF", "ETH2")
	if err != nil {
		t.Fatalf("GetAccessPointPort: %v", err)
	}
	if !p.UplinkPort || p.SupportVLAN {
		t.Fatalf("ETH2 should read as the uplink without VLAN support, got %+v", p)
	}

	// ETH20 and ETH2X are other ports, not ETH2 with a suffix.
	for _, other := range []string{"ETH20", "ETH2X"} {
		ctl.misc = map[string]any{"uplinkPort": other, "supportUplinkPortConfig": false, "lanVlanPorts": []any{"ETH2"}}
		if p, err := c.GetAccessPointPort(context.Background(), "site-1", "AA-BB-CC-DD-EE-FF", "ETH2"); err != nil || p.UplinkPort || !p.SupportVLAN {
			t.Fatalf("%s: ETH2 should read as a VLAN-capable non-uplink, got %+v, %v", other, p, err)
		}
	}

	ctl.misc = map[string]any{"uplinkPort": float64(1), "lanVlanPorts": []any{"ETH2"}}
	p, err = c.GetAccessPointPort(context.Background(), "site-1", "AA-BB-CC-DD-EE-FF", "ETH2")
	if err != nil {
		t.Fatalf("an unknown deviceMisc shape should not fail a read, got %v", err)
	}
	if p.SupportVLAN {
		t.Fatalf("with deviceMisc unread, support_vlan should read false, as writes are refused")
	}
}

// An AP that reports no ports at all may be disconnected or upgrading: that is
// not a port gone, so a resource keeps it in state.
func TestGetAccessPointPortNoPortsIsNotNotFound(t *testing.T) {
	c, ctl := newAPPortController(t, merge)
	ctl.ports = nil

	_, err := c.GetAccessPointPort(context.Background(), "site-1", "AA-BB-CC-DD-EE-FF", "ETH2")
	if !errors.Is(err, ErrAccessPointNoPorts) || errors.Is(err, ErrAccessPointPortNotFound) {
		t.Fatalf("expected ErrAccessPointNoPorts and not ErrAccessPointPortNotFound, got %v", err)
	}
}

func TestGetAccessPointPortNotFound(t *testing.T) {
	c, _ := newAPPortController(t, merge)

	_, err := c.GetAccessPointPort(context.Background(), "site-1", "AA-BB-CC-DD-EE-FF", "ETH9")
	if !errors.Is(err, ErrAccessPointPortNotFound) {
		t.Fatalf("expected ErrAccessPointPortNotFound, got %v", err)
	}
}
