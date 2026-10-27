// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package nodelink

import (
	"testing"

	networkv1 "github.com/gardenlinux/feos/go/gen/feos/network/v1"
	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	toolv1 "github.com/gardenlinux/feos/go/gen/feos/tool/v1"
	volumev1 "github.com/gardenlinux/feos/go/gen/feos/volume/v1"
)

// The calls that prepare a node for an environment's pod (its volume, its
// security groups) are lifecycle calls, as the pod calls of ops are: the
// drain timeout never cuts them, and no budget refuses them.
func TestUnaryKind(t *testing.T) {
	for _, tc := range []struct {
		method string
		want   callKind
	}{
		{podv1.PodService_CreatePod_FullMethodName, callLifecycle},
		{podv1.PodService_DeletePod_FullMethodName, callLifecycle},
		{podv1.PodService_GetPod_FullMethodName, callPlain},
		{volumev1.VolumeService_CreateVolume_FullMethodName, callLifecycle},
		{volumev1.VolumeService_DeleteVolume_FullMethodName, callLifecycle},
		{volumev1.VolumeService_GetVolume_FullMethodName, callLifecycle},
		{volumev1.VolumeService_ListVolumes_FullMethodName, callLifecycle},
		{networkv1.SecurityGroupService_PutSecurityGroup_FullMethodName, callLifecycle},
		{networkv1.SecurityGroupService_UpdateSecurityGroup_FullMethodName, callLifecycle},
		{networkv1.SecurityGroupService_DeleteSecurityGroup_FullMethodName, callLifecycle},
		{networkv1.SecurityGroupService_GetSecurityGroup_FullMethodName, callLifecycle},
		{networkv1.SecurityGroupService_ListSecurityGroups_FullMethodName, callLifecycle},
		{networkv1.MeshService_PutMeshPeers_FullMethodName, callPlain},
		{toolv1.ToolService_Read_FullMethodName, callTool},
	} {
		if got := unaryKind(tc.method); got != tc.want {
			t.Errorf("%s: kind %d, want %d", tc.method, got, tc.want)
		}
	}
}
