package driver

import (
	"bytes"
	"context"
	"encoding/json"

	resourceapi "k8s.io/api/resource/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// DeviceStatusData is published to RC.Status.Devices[].Data for operator
// introspection via `kubectl describe resourceclaim`.
type DeviceStatusData struct {
	VNI   int `json:"vni"`
	SvcID int `json:"svc_id,omitempty"`
	// VF is the VF backing a VF-mode parent allocation; empty in PF mode.
	VF string `json:"vf,omitempty"`
}

// RecordSvcID publishes the VNI, svc_id and (VF mode) the backing VF for one
// allocation of a claim to the claim's status. Called by the NRI plugin after
// CreateService succeeds. Best-effort: any API failure is logged and ignored —
// informational only.
func (d *Driver) RecordSvcID(ctx context.Context, alloc PodAllocation, svcID int) {
	data := DeviceStatusData{VNI: alloc.VNI, SvcID: svcID}
	if alloc.ParentName != "" {
		data.VF = alloc.DeviceName
	}
	d.writeDeviceStatus(ctx, alloc.ClaimNamespace, alloc.ClaimName, alloc.ClaimUID,
		map[pendingKey]DeviceStatusData{{deviceName: alloc.AllocatedDevice(), shareID: alloc.ShareID}: data})
}

// writeDeviceStatus best-effort merges status entries, keyed by allocation
// result (device and ShareID), into the claim's status. Retries on conflict
// because the ClaimBinder concurrently writes BindingConditions on the same
// subresource.
func (d *Driver) writeDeviceStatus(
	ctx context.Context,
	claimNs, claimName string,
	claimUID types.UID,
	deviceData map[pendingKey]DeviceStatusData,
) {
	const maxRetries = 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		latest, err := d.kubeClient.ResourceV1().ResourceClaims(claimNs).Get(
			ctx, claimName, metav1.GetOptions{})
		if err != nil {
			if !k8serrors.IsNotFound(err) {
				d.logf("warning: getting claim %s/%s for status: %v", claimNs, claimName, err)
			}
			return
		}
		if latest.UID != claimUID || latest.Status.Allocation == nil {
			return
		}

		changed := false
		for i := range latest.Status.Allocation.Devices.Results {
			r := &latest.Status.Allocation.Devices.Results[i]
			if r.Driver != d.driverName {
				continue
			}
			data, ok := deviceData[pendingKey{deviceName: r.Device, shareID: shareIDOf(*r)}]
			if !ok {
				continue
			}
			payload, err := json.Marshal(data)
			if err != nil {
				continue
			}
			if upsertDeviceData(latest, r, &runtime.RawExtension{Raw: payload}) {
				changed = true
			}
		}
		if !changed {
			return
		}

		_, err = d.kubeClient.ResourceV1().ResourceClaims(claimNs).UpdateStatus(
			ctx, latest, metav1.UpdateOptions{})
		if err == nil {
			return
		}
		if k8serrors.IsConflict(err) {
			continue
		}
		d.logf("warning: updating status for %s/%s: %v", claimNs, claimName, err)
		return
	}
	d.logf("warning: gave up updating status for %s/%s after %d attempts",
		claimNs, claimName, maxRetries)
}

// upsertDeviceData sets Data on the matching AllocatedDeviceStatus entry,
// creating it if absent. The (Driver, Pool, Device, ShareID) tuple must
// match the allocation result — the API server validates this.
func upsertDeviceData(
	claim *resourceapi.ResourceClaim,
	result *resourceapi.DeviceRequestAllocationResult,
	data *runtime.RawExtension,
) bool {
	resultShareID := ""
	if result.ShareID != nil {
		resultShareID = string(*result.ShareID)
	}
	for i, dev := range claim.Status.Devices {
		if dev.Driver != result.Driver || dev.Pool != result.Pool || dev.Device != result.Device {
			continue
		}
		devShareID := ""
		if dev.ShareID != nil {
			devShareID = *dev.ShareID
		}
		if devShareID != resultShareID {
			continue
		}
		if dev.Data != nil && bytes.Equal(dev.Data.Raw, data.Raw) {
			return false
		}
		claim.Status.Devices[i].Data = data
		return true
	}
	entry := resourceapi.AllocatedDeviceStatus{
		Driver: result.Driver,
		Pool:   result.Pool,
		Device: result.Device,
		Data:   data,
	}
	if result.ShareID != nil {
		s := string(*result.ShareID)
		entry.ShareID = &s
	}
	claim.Status.Devices = append(claim.Status.Devices, entry)
	return true
}
