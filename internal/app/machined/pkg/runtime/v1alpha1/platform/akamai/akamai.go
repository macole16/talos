// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package akamai contains the Akamai implementation of the [platform.Platform].
package akamai

import (
	"cmp"
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	akametadata "github.com/linode/go-metadata"
	"github.com/siderolabs/go-procfs/procfs"

	"github.com/siderolabs/talos/internal/app/machined/pkg/runtime"
	"github.com/siderolabs/talos/internal/app/machined/pkg/runtime/v1alpha1/platform/errors"
	"github.com/siderolabs/talos/internal/app/machined/pkg/runtime/v1alpha1/platform/internal/netutils"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/imager/quirks"
	"github.com/siderolabs/talos/pkg/machinery/nethelpers"
	"github.com/siderolabs/talos/pkg/machinery/resources/network"
	runtimeres "github.com/siderolabs/talos/pkg/machinery/resources/runtime"
)

// Akamai is the concrete type that implements the platform.Platform interface.
type Akamai struct{}

// Name implements the platform.Platform interface.
func (a *Akamai) Name() string {
	return "akamai"
}

// ParseMetadata converts Akamai platform metadata into platform network config.
//
// linkNames holds the host's physical link names in PCI order (see
// physicalLinkNames); metadata interface N maps to linkNames[N]. The returned
// bool reports whether a link could not be resolved yet and the caller should
// reconcile and retry.
func (a *Akamai) ParseMetadata(
	metadata *akametadata.InstanceData,
	interfaceAddresses *NetworkData,
	linkNames []string,
) (*runtime.PlatformNetworkConfig, bool, error) {
	networkConfig := &runtime.PlatformNetworkConfig{}

	if metadata.Label != "" {
		hostnameSpec := network.HostnameSpecSpec{
			ConfigLayer: network.ConfigPlatform,
		}

		if err := hostnameSpec.ParseFQDN(metadata.Label); err != nil {
			return nil, false, err
		}

		networkConfig.Hostnames = append(networkConfig.Hostnames, hostnameSpec)
	}

	// The instance-wide public/private/IPv6 addresses live on the public
	// interface, which is the first physical NIC in PCI order.
	publicLink, resolved := resolveLink(linkNames, 0)
	needsReconcile := !resolved

	if err := configurePublicAddresses(networkConfig, interfaceAddresses, publicLink); err != nil {
		return nil, false, err
	}

	// Configure the node's secondary VPC interfaces so no competing default route
	// is installed on them (see configureVPCInterfaces for the mechanism / #13708).
	if configureVPCInterfaces(networkConfig, interfaceAddresses.Interfaces, linkNames) {
		needsReconcile = true
	}

	networkConfig.Metadata = &runtimeres.PlatformMetadataSpec{
		Platform:     a.Name(),
		Hostname:     metadata.Label,
		Region:       metadata.Region,
		InstanceType: metadata.Type,
		InstanceID:   strconv.Itoa(metadata.ID),
		ProviderID:   fmt.Sprintf("linode://%d", metadata.ID),
		Tags:         convertTagsFromAkamai(metadata.Tags),
	}

	return networkConfig, needsReconcile, nil
}

// configurePublicAddresses adds the instance-wide public, private and IPv6
// addresses (all on the public link) together with the IPv6 link-local route
// and the external IPs.
func configurePublicAddresses(networkConfig *runtime.PlatformNetworkConfig, interfaceAddresses *NetworkData, publicLink string) error {
	publicIPs := make(
		[]string,
		0,
		len(interfaceAddresses.IPv4.Public)+len(interfaceAddresses.IPv6.Ranges),
	)

	for _, iface := range interfaceAddresses.IPv4.Public {
		publicIPs = append(publicIPs, iface.Addr().String())
		networkConfig.Addresses = append(
			networkConfig.Addresses,
			network.AddressSpecSpec{
				ConfigLayer: network.ConfigPlatform,
				LinkName:    publicLink,
				Address:     iface,
				Scope:       nethelpers.ScopeGlobal,
				Flags:       nethelpers.AddressFlags(nethelpers.AddressPermanent),
				Family:      nethelpers.FamilyInet4,
			},
		)
	}

	for _, iface := range interfaceAddresses.IPv4.Private {
		networkConfig.Addresses = append(
			networkConfig.Addresses,
			network.AddressSpecSpec{
				ConfigLayer: network.ConfigPlatform,
				LinkName:    publicLink,
				Address:     iface,
				Scope:       nethelpers.ScopeGlobal,
				Flags:       nethelpers.AddressFlags(nethelpers.AddressPermanent),
				Family:      nethelpers.FamilyInet4,
			},
		)
	}

	for _, iface := range interfaceAddresses.IPv6.Ranges {
		publicIPs = append(publicIPs, iface.Addr().String())

		networkConfig.Addresses = append(
			networkConfig.Addresses,
			network.AddressSpecSpec{
				ConfigLayer: network.ConfigPlatform,
				LinkName:    publicLink,
				Address:     iface,
				Scope:       nethelpers.ScopeGlobal,
				Flags:       nethelpers.AddressFlags(nethelpers.AddressManagementTemp),
				Family:      nethelpers.FamilyInet6,
			},
		)
	}

	networkConfig.Addresses = append(
		networkConfig.Addresses,
		network.AddressSpecSpec{
			ConfigLayer: network.ConfigPlatform,
			LinkName:    publicLink,
			Address:     interfaceAddresses.IPv6.LinkLocal,
			Scope:       nethelpers.ScopeLink,
			Family:      nethelpers.FamilyInet6,
		},
	)

	ipv6gw, err := netip.ParseAddr(
		strings.Split(interfaceAddresses.IPv6.LinkLocal.String(), ":")[0] + "::1",
	)
	if err != nil {
		return err
	}

	route := network.RouteSpecSpec{
		ConfigLayer: network.ConfigPlatform,
		Gateway:     ipv6gw,
		OutLinkName: publicLink,
		Destination: interfaceAddresses.IPv6.LinkLocal,
		Table:       nethelpers.TableMain,
		Protocol:    nethelpers.ProtocolStatic,
		Type:        nethelpers.TypeUnicast,
		Family:      nethelpers.FamilyInet6,
		Priority:    1024,
	}

	route.Normalize()

	networkConfig.Routes = append(networkConfig.Routes, route)

	for _, ipStr := range publicIPs {
		if ip, err := netip.ParseAddr(ipStr); err == nil {
			networkConfig.ExternalIPs = append(networkConfig.ExternalIPs, ip)
		}
	}

	return nil
}

// Configuration implements the platform.Platform interface.
func (a *Akamai) Configuration(ctx context.Context, r state.State) ([]byte, error) {
	if err := netutils.Wait(ctx, r); err != nil {
		return nil, err
	}

	metadataClient, err := akametadata.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("new metadata client: %w", err)
	}

	userData, err := metadataClient.GetUserData(ctx)
	if err != nil {
		return nil, fmt.Errorf("get user data: %w", err)
	}

	if userData == "" {
		return nil, errors.ErrNoConfigSource
	}

	return []byte(userData), nil
}

// Mode implements the platform.Platform interface.
func (a *Akamai) Mode() runtime.Mode {
	return runtime.ModeCloud
}

// KernelArgs implements the runtime.Platform interface.
func (a *Akamai) KernelArgs(string, quirks.Quirks) procfs.Parameters {
	return []*procfs.Parameter{
		procfs.NewParameter("console").Append("ttyS0").Append("tty0").Append("tty1"),
		procfs.NewParameter(constants.KernelParamNetIfnames).Append("0"),
	}
}

// NetworkConfiguration implements the runtime.Platform interface.
func (a *Akamai) NetworkConfiguration(ctx context.Context, st state.State, ch chan<- *runtime.PlatformNetworkConfig) error {
	// Wait for the network devices to be enumerated so link names can be resolved.
	if err := netutils.WaitForDevicesReady(ctx, st); err != nil {
		return fmt.Errorf("error waiting for devices to be ready: %w", err)
	}

	metadata, interfaceAddresses, err := fetchNetworkMetadata(ctx)
	if err != nil {
		return err
	}

	return a.reconcileNetworkConfig(ctx, st, metadata, interfaceAddresses, ch)
}

// fetchNetworkMetadata retrieves the instance and network metadata. The network
// data is fetched from /v1/network raw (via the client's authenticated request)
// rather than through GetNetwork, because go-metadata v0.2.4 does not model the
// per-interface "vpc" object the driver needs to configure VPC interfaces.
func fetchNetworkMetadata(ctx context.Context) (*akametadata.InstanceData, *NetworkData, error) {
	metadataClient, err := akametadata.NewClient(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("new metadata client: %w", err)
	}

	metadata, err := metadataClient.GetInstance(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("get instance data: %w", err)
	}

	interfaceAddresses := &NetworkData{}

	resp, err := metadataClient.R(ctx).SetResult(interfaceAddresses).Get("network")
	if err != nil {
		return nil, nil, fmt.Errorf("get network data: %w", err)
	}

	if resp.IsError() {
		return nil, nil, fmt.Errorf("get network data: unexpected status %s", resp.Status())
	}

	return metadata, interfaceAddresses, nil
}

// reconcileNetworkConfig retries the metadata->config mapping until every
// physical link is enumerated, exporting the best-effort config on each pass
// (mirrors the openstack driver).
func (a *Akamai) reconcileNetworkConfig(
	ctx context.Context,
	st state.State,
	metadata *akametadata.InstanceData,
	interfaceAddresses *NetworkData,
	ch chan<- *runtime.PlatformNetworkConfig,
) error {
	bckoff := backoff.NewExponentialBackOff()

	for {
		hostInterfaces, err := safe.StateListAll[*network.LinkStatus](ctx, st)
		if err != nil {
			return fmt.Errorf("error listing host interfaces: %w", err)
		}

		linkNames := physicalLinkNames(slices.Collect(hostInterfaces.All()))

		networkConfig, needsReconcile, err := a.ParseMetadata(metadata, interfaceAddresses, linkNames)
		if err != nil {
			return fmt.Errorf("parse metadata: %w", err)
		}

		select {
		case ch <- networkConfig:
		case <-ctx.Done():
			return ctx.Err()
		}

		if !needsReconcile {
			return nil
		}

		nextBackoff := bckoff.NextBackOff()
		if nextBackoff == backoff.Stop {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(nextBackoff):
		}
	}
}

// physicalLinkNames returns the physical host link names ordered by PCI bus path.
// Linode metadata carries no MAC addresses, but virtio NICs are attached to PCI
// slots in configuration order (public first, then VPC), so bus-path order
// matches the metadata interface order. Resolving names from the host this way is
// rename-invariant: it yields eth0/eth1 or ens3/ens4 alike, avoiding the
// hardcoded-name mismatch that leaves link config inert on predictable-naming
// kernels.
func physicalLinkNames(hostInterfaces []*network.LinkStatus) []string {
	physical := make([]*network.LinkStatus, 0, len(hostInterfaces))

	for _, link := range hostInterfaces {
		if link.TypedSpec().Physical() {
			physical = append(physical, link)
		}
	}

	slices.SortFunc(physical, func(a, b *network.LinkStatus) int {
		return cmp.Compare(a.TypedSpec().BusPath, b.TypedSpec().BusPath)
	})

	names := make([]string, 0, len(physical))
	for _, link := range physical {
		names = append(names, link.Metadata().ID())
	}

	return names
}

// resolveLink returns the host link name for the metadata interface at idx,
// resolved from the host by PCI order (see physicalLinkNames). The bool is false
// when the link is not yet enumerated; the caller should reconcile and retry
// rather than rely on the "eth%d" fallback.
func resolveLink(linkNames []string, idx int) (string, bool) {
	if idx >= 0 && idx < len(linkNames) {
		return linkNames[idx], true
	}

	return fmt.Sprintf("eth%d", idx), false
}

// primaryInterfaceIndex returns the index of the node's primary interface — the
// first non-VLAN interface, which Linode treats as the default-route holder. It
// returns -1 when there are no interfaces (the legacy single-NIC case).
func primaryInterfaceIndex(interfaces []NetworkInterface) int {
	for i, iface := range interfaces {
		if iface.Purpose != "vlan" {
			return i
		}
	}

	return -1
}

// configureVPCInterfaces configures the node's secondary VPC interfaces so Talos
// does not install a competing default route on them. Talos runs its default
// DHCPv4 operator on every physical NIC that has no explicit link configuration;
// on a dual-homed node the VPC DHCP server then hands out its own gateway as a
// default route at the same metric as the primary interface's, and when that
// (internet-less) route wins the node silently loses egress while still
// appearing to run (siderolabs/talos#13708). Emitting a platform link spec for
// the interface suppresses that default operator, so a secondary VPC interface
// is given only its static address: no DHCPv4 operator and no default route.
//
// The metadata service has no default-route indicator, so the primary interface
// is left untouched, keeping its DHCPv4-provided default route — on a VPC-only
// node, where the primary is itself a VPC interface reaching the internet via
// 1:1 NAT, that is the node's only egress.
//
// The returned bool reports whether a VPC link could not be resolved yet.
func configureVPCInterfaces(networkConfig *runtime.PlatformNetworkConfig, interfaces []NetworkInterface, linkNames []string) bool {
	needsReconcile := false
	primaryIdx := primaryInterfaceIndex(interfaces)

	for idx, iface := range interfaces {
		if idx == primaryIdx || iface.Purpose != "vpc" || iface.VPC == nil || iface.VPC.Subnet.IPv4 == nil {
			continue
		}

		// The metadata service reports the VPC address as a /32; pair it with the
		// subnet's prefix length so the static address also installs the connected
		// subnet route used for node-to-node traffic within the VPC. Skip the
		// interface if the metadata is incomplete (e.g. a missing subnet range).
		vpcIPv4 := iface.VPC.Subnet.IPv4

		address := netip.PrefixFrom(vpcIPv4.InstanceAddress.Addr(), vpcIPv4.SubnetRange.Bits())
		if !address.IsValid() {
			continue
		}

		// Skip (and reconcile) rather than emit a possibly-wrong spec if the host
		// link is not enumerated yet.
		linkName, resolved := resolveLink(linkNames, idx)
		if !resolved {
			needsReconcile = true

			continue
		}

		networkConfig.Links = append(networkConfig.Links,
			network.LinkSpecSpec{
				Name:        linkName,
				Up:          true,
				ConfigLayer: network.ConfigPlatform,
			},
		)

		networkConfig.Addresses = append(networkConfig.Addresses,
			network.AddressSpecSpec{
				ConfigLayer: network.ConfigPlatform,
				LinkName:    linkName,
				Address:     address,
				Scope:       nethelpers.ScopeGlobal,
				Flags:       nethelpers.AddressFlags(nethelpers.AddressPermanent),
				Family:      nethelpers.FamilyInet4,
			},
		)
	}

	return needsReconcile
}

// convertTagsFromAkamai converts Akamai instance tags into the format expected by PlatformMetadata.
func convertTagsFromAkamai(akamaiTags []string) map[string]string {
	var platformMetadataTags map[string]string
	if len(akamaiTags) > 0 {
		platformMetadataTags = make(map[string]string, len(akamaiTags))
		for _, key := range akamaiTags {
			platformMetadataTags[key] = ""
		}
	}

	return platformMetadataTags
}
