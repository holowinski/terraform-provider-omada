// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/wncservices/terraform-provider-omada/internal/omada"
)

var (
	_ resource.Resource                   = &accessPointPortResource{}
	_ resource.ResourceWithConfigure      = &accessPointPortResource{}
	_ resource.ResourceWithImportState    = &accessPointPortResource{}
	_ resource.ResourceWithValidateConfig = &accessPointPortResource{}
)

func NewAccessPointPortResource() resource.Resource { return &accessPointPortResource{} }

type accessPointPortResource struct{ data *providerData }

type accessPointPortResourceModel struct {
	ID     types.String `tfsdk:"id"`
	Site   types.String `tfsdk:"site"`
	SiteID types.String `tfsdk:"site_id"`
	APMAC  macValue     `tfsdk:"ap_mac"`
	Port   types.String `tfsdk:"port"`

	Name        types.String `tfsdk:"name"`
	Uplink      types.Bool   `tfsdk:"uplink"`
	SupportVLAN types.Bool   `tfsdk:"support_vlan"`

	VLANEnable types.Bool  `tfsdk:"vlan_enable"`
	VLANID     types.Int64 `tfsdk:"vlan_id"`
}

func (r *accessPointPortResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_access_point_port"
}

func (r *accessPointPortResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the VLAN of one Ethernet (LAN) port on an adopted access point — the port " +
			"an EAP passes a wired device through, such as `ETH2` on an EAP245. The VLAN is set by id, as the " +
			"controller's *VLAN ID* option does; binding a port to a network profile, and tagged VLANs, are " +
			"not modelled.\n\n" +
			"~> **The write is inferred, not confirmed on hardware.** The port's shape was read from a live " +
			"controller; the write sends the body another Omada client sends. The AP is read back after " +
			"every write: a VLAN the controller did not store, or any other port setting that moved, fails " +
			"the apply. That proves what the controller stored, not that the AP applied it.\n\n" +
			"~> **A port is physical: it cannot be created or destroyed.** Terraform *adopts* the existing " +
			"port. Removing this resource from your configuration leaves the port exactly as last applied — " +
			"Terraform forgets it, the AP does not. Changing a port's VLAN moves whatever is plugged into it " +
			"onto another network.\n\n" +
			"~> **The AP's uplink port is refused**: its VLAN carries the AP's own management traffic, so a " +
			"wrong value there cuts the AP off from the controller. So is any port the AP does not report " +
			"as VLAN-capable, and every port of an AP that does not report its uplink as fixed " +
			"(`supportUplinkPortConfig` false). An EAP245 lists only `ETH2`, which is VLAN-capable, and " +
			"has a fixed uplink.\n\n" +
			"Every settable attribute is optional: whatever you leave out keeps its current value on the " +
			"controller, and will never report drift. The port is written only when a configured value " +
			"differs from the device.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`<ap mac>/<port>`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"site": schema.StringAttribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Site name. Defaults to the primary site. Changing forces replacement.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(), stringplanmodifier.RequiresReplace(),
				},
			},
			"site_id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"ap_mac": schema.StringAttribute{
				CustomType: macType{},
				Required:   true,
				MarkdownDescription: "MAC address of the access point, in any common spelling. " +
					"See the `omada_devices` data source.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"port": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The port's name as the controller reports it, e.g. `ETH2`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},

			"name": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The port's label in the controller. Read-only.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"uplink": schema.BoolAttribute{
				Computed: true,
				MarkdownDescription: "Whether the controller marks this as the port the AP is connected " +
					"through, on the port or in the AP's device details. Read-only; such a port is never " +
					"written. An EAP245 does not list its uplink at all, so it cannot be managed here.",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"support_vlan": schema.BoolAttribute{
				Computed: true,
				MarkdownDescription: "Whether this port's VLAN can be written, unless `uplink` is true: the " +
					"port and the AP's list of VLAN-capable ports both say it can, and the AP's uplink is fixed " +
					"(`supportUplinkPortConfig` false). Read-only.",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},

			"vlan_enable": schema.BoolAttribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Put untagged traffic on this port into `vlan_id`. Off, the port " +
					"carries whatever the AP's uplink carries untagged.",
			},
			"vlan_id": schema.Int64Attribute{
				Optional: true, Computed: true,
				MarkdownDescription: "The VLAN untagged traffic on this port goes into when `vlan_enable` " +
					"is on: the `vlan_id` of the `omada_network` the port should be on. Reads `0` on a port " +
					"whose VLAN has never been set, so enabling one there needs this too. Requires " +
					"`vlan_enable = true`: while the VLAN is off the id has no effect, and the controller " +
					"may not keep it.",
				Validators: []validator.Int64{int64validator.Between(1, 4094)},
			},
		},
	}
}

func (r *accessPointPortResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg accessPointPortResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if cfg.VLANID.IsNull() || cfg.VLANID.IsUnknown() || cfg.VLANEnable.IsUnknown() {
		return
	}
	if cfg.VLANEnable.IsNull() || !cfg.VLANEnable.ValueBool() {
		resp.Diagnostics.AddAttributeError(path.Root("vlan_id"),
			"vlan_id requires vlan_enable = true",
			"while the port's VLAN is off its id has no effect, and the controller may not keep it")
	}
}

func (r *accessPointPortResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("got %T", req.ProviderData))
		return
	}
	r.data = data
}

func (r *accessPointPortResource) siteName(m accessPointPortResourceModel) string {
	if !m.Site.IsNull() && m.Site.ValueString() != "" {
		return m.Site.ValueString()
	}
	return r.data.defaultSite
}

func (r *accessPointPortResource) resolveSite(ctx context.Context, m accessPointPortResourceModel, diags *diagSink) (string, string) {
	site, err := r.data.client.ResolveSite(ctx, r.siteName(m))
	if err != nil {
		diags.AddError("Unable to resolve site", err.Error())
		return "", ""
	}
	return site.ID, site.Name
}

// accessPointPortUpdate overlays the configured attributes on the port as it
// is. It returns nil when nothing differs, so an unchanged port is never
// written.
func accessPointPortUpdate(cur omada.AccessPointPort, plan accessPointPortResourceModel) *omada.AccessPointPortUpdate {
	next := omada.AccessPointPortUpdate{
		LANPort:         cur.LANPort,
		LocalVLANEnable: cur.LocalVLANEnable,
		LocalVLANID:     cur.LocalVLANID,
	}
	if known(plan.VLANEnable) {
		next.LocalVLANEnable = plan.VLANEnable.ValueBool()
	}
	if known(plan.VLANID) {
		next.LocalVLANID = int(plan.VLANID.ValueInt64())
	}
	if next.LocalVLANEnable == cur.LocalVLANEnable && next.LocalVLANID == cur.LocalVLANID {
		return nil
	}
	return &next
}

func (r *accessPointPortResource) refresh(p *omada.AccessPointPort, m *accessPointPortResourceModel) {
	m.ID = types.StringValue(omada.NormalizeMAC(m.APMAC.ValueString()) + "/" + p.LANPort)
	m.Port = types.StringValue(p.LANPort)
	m.Name = types.StringValue(p.Name)
	m.Uplink = types.BoolValue(p.UplinkPort)
	m.SupportVLAN = types.BoolValue(p.SupportVLAN)
	m.VLANEnable = types.BoolValue(p.LocalVLANEnable)
	m.VLANID = types.Int64Value(int64(p.LocalVLANID))
}

// apply is the shared body of Create and Update.
func (r *accessPointPortResource) apply(ctx context.Context, plan *accessPointPortResourceModel, diags *diagSink) {
	siteID, siteName := r.resolveSite(ctx, *plan, diags)
	if siteID == "" {
		return
	}
	mac := plan.APMAC.ValueString()

	cur, err := r.data.client.GetAccessPointPort(ctx, siteID, mac, plan.Port.ValueString())
	if err != nil {
		diags.AddError("Unable to read access point port", err.Error())
		return
	}
	if body := accessPointPortUpdate(*cur, *plan); body != nil {
		if cur, err = r.data.client.UpdateAccessPointPort(ctx, siteID, mac, *body); err != nil {
			diags.AddError("Unable to update access point port", err.Error())
			return
		}
	}
	r.refresh(cur, plan)
	plan.SiteID = types.StringValue(siteID)
	plan.Site = types.StringValue(siteName)
}

// Create adopts the existing port rather than making one.
func (r *accessPointPortResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan accessPointPortResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.apply(ctx, &plan, &diagSink{&resp.Diagnostics})
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *accessPointPortResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state accessPointPortResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	siteID, siteName := r.resolveSite(ctx, state, &diagSink{&resp.Diagnostics})
	if resp.Diagnostics.HasError() {
		return
	}
	mac := state.APMAC.ValueString()
	cur, err := r.data.client.GetAccessPointPort(ctx, siteID, mac, state.Port.ValueString())
	if errors.Is(err, omada.ErrAccessPointNoPorts) {
		// A disconnected or upgrading AP may list no ports for a while. Keep
		// the port as last read rather than fail every plan until it is back,
		// unless the AP has left the site, or there is nothing read yet.
		if _, derr := r.data.client.GetDevice(ctx, siteID, mac); errors.Is(derr, omada.ErrDeviceNotFound) {
			resp.State.RemoveResource(ctx)
			return
		}
		if state.ID.IsNull() {
			resp.Diagnostics.AddError("Unable to read access point port", err.Error())
			return
		}
		resp.Diagnostics.AddWarning("Access point port not read",
			err.Error()+"; the port is kept as last read until the AP lists its ports again.")
		return
	}
	if err != nil {
		// Drop the port only once it, or its AP, is gone from the site. A
		// failed request keeps it, so an outage never plans a re-adoption.
		gone := errors.Is(err, omada.ErrAccessPointPortNotFound)
		if gone && state.ID.IsNull() {
			// An import naming a port the AP does not have: say which it has.
			resp.Diagnostics.AddError("Unable to read access point port", err.Error())
			return
		}
		if !gone {
			_, derr := r.data.client.GetDevice(ctx, siteID, mac)
			gone = errors.Is(derr, omada.ErrDeviceNotFound)
		}
		if gone {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Unable to read access point port", err.Error())
		return
	}
	r.refresh(cur, &state)
	state.SiteID = types.StringValue(siteID)
	state.Site = types.StringValue(siteName)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *accessPointPortResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state accessPointPortResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.SiteID = state.SiteID
	r.apply(ctx, &plan, &diagSink{&resp.Diagnostics})
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete drops the port from state without touching the AP.
func (r *accessPointPortResource) Delete(_ context.Context, _ resource.DeleteRequest, resp *resource.DeleteResponse) {
	resp.Diagnostics.AddWarning(
		"Access point port left as configured",
		"Ports are physical and cannot be deleted, so Terraform has only stopped managing this one. "+
			"Its VLAN on the access point is unchanged — reset it by hand, or re-import the port, if "+
			"that is not what you wanted.",
	)
}

// ImportState takes "<ap mac>/<port>", optionally prefixed with a site:
// "<site>/<ap mac>/<port>".
func (r *accessPointPortResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(req.ID, "/")
	if len(parts) == 3 {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("site"), parts[0])...)
		parts = parts[1:]
	}
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		resp.Diagnostics.AddError(
			"Unexpected import id",
			fmt.Sprintf("expected \"<ap mac>/<port>\" or \"<site>/<ap mac>/<port>\", got %q", req.ID),
		)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("ap_mac"), omada.NormalizeMAC(parts[0]))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("port"), parts[1])...)
}
