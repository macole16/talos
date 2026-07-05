// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package akamai

import "net/netip"

// NetworkData is a partial view of the Akamai (Linode) metadata service
// /v1/network response. It mirrors the fields the driver consumes, plus the
// per-interface "vpc" object that github.com/linode/go-metadata (v0.2.4) does
// not model. The metadata service exposes no default-route/primary indicator
// on its interface entries, so the driver leaves the primary interface alone
// and configures only secondary VPC interfaces (see ParseMetadata).
type NetworkData struct {
	Interfaces []NetworkInterface `json:"interfaces"`
	IPv4       NetworkIPv4        `json:"ipv4"`
	IPv6       NetworkIPv6        `json:"ipv6"`
}

// NetworkIPv4 holds the instance-wide IPv4 addresses (all on the public link).
type NetworkIPv4 struct {
	Public  []netip.Prefix `json:"public"`
	Private []netip.Prefix `json:"private"`
}

// NetworkIPv6 holds the instance-wide IPv6 addresses (all on the public link).
type NetworkIPv6 struct {
	LinkLocal netip.Prefix   `json:"link_local"`
	Ranges    []netip.Prefix `json:"ranges"`
}

// NetworkInterface is a single /v1/network interface entry.
type NetworkInterface struct {
	Purpose string      `json:"purpose"`
	VPC     *VPCNetwork `json:"vpc"`
}

// VPCNetwork describes the VPC an interface is attached to.
type VPCNetwork struct {
	Subnet VPCSubnet `json:"subnet"`
}

// VPCSubnet describes the VPC subnet an interface is attached to.
type VPCSubnet struct {
	IPv4 *VPCSubnetIPv4 `json:"ipv4"`
}

// VPCSubnetIPv4 describes the interface's IPv4 addressing within a VPC subnet.
type VPCSubnetIPv4 struct {
	// InstanceAddress is this node's address on the VPC subnet, reported as a /32
	// (e.g. "10.20.0.10/32"). SubnetRange is the subnet's CIDR (e.g.
	// "10.20.0.0/24"); its prefix length is used to widen InstanceAddress so the
	// configured address installs the connected subnet route.
	InstanceAddress netip.Prefix `json:"instance_address"`
	SubnetRange     netip.Prefix `json:"subnet_range"`
}
