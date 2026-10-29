package csi

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// identity is the CSI identity service both plugins serve (csi.md, The
// driver): the driver's name and version, the plugin's capabilities and a
// liveness probe. Only the controller plugin advertises the controller
// service.
type identity struct {
	csi.UnimplementedIdentityServer
	controller bool
}

func newIdentity(controller bool) *identity {
	return &identity{controller: controller}
}

func (i *identity) GetPluginInfo(
	ctx context.Context,
	req *csi.GetPluginInfoRequest,
) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{
		Name:          DriverName,
		VendorVersion: Version,
	}, nil
}

func (i *identity) GetPluginCapabilities(
	ctx context.Context,
	req *csi.GetPluginCapabilitiesRequest,
) (*csi.GetPluginCapabilitiesResponse, error) {
	reply := &csi.GetPluginCapabilitiesResponse{}
	if i.controller {
		reply.Capabilities = append(reply.Capabilities, &csi.PluginCapability{
			Type: &csi.PluginCapability_Service_{
				Service: &csi.PluginCapability_Service{
					Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
				},
			},
		})
	}
	return reply, nil
}

// Probe answers ready: a plugin that serves is a plugin that is ready, since
// it holds no state of its own to warm up. A gateway that does not answer
// shows on the calls that need it, not here.
func (i *identity) Probe(
	ctx context.Context,
	req *csi.ProbeRequest,
) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}
