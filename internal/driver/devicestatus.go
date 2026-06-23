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
}

// RecordSvcID publishes the VNI and svc_id for one (claim, device) pair to
// the claim's status. Called by the NRI plugin after CreateService succeeds.
// Best-effort: any API failure is logged and ignored — informational only.
func (d *Driver) RecordSvcID(
	ctx context.Context,
	claimUID types.UID,
	claimNs, claimName, deviceName string,
	vni, svcID int,
) {
	d.writeDeviceStatus(ctx, claimNs, claimName, claimUID,
		map[string]DeviceStatusData{deviceName: {VNI: vni, SvcID: svcID}})
}

// writeDeviceStatus best-effort merges per-device status entries into the
// claim's status. Retries on conflict because the ClaimBinder concurrently
// writes BindingConditions on the same subresource.
func (d *Driver) writeDeviceStatus(
	ctx context.Context,
	claimNs, claimName string,
	claimUID types.UID,
	deviceData map[string]DeviceStatusData,
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
			data, ok := deviceData[r.Device]
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
