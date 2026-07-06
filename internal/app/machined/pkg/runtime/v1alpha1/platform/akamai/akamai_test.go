// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package akamai_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/state"
	"github.com/cosi-project/runtime/pkg/state/impl/inmem"
	"github.com/cosi-project/runtime/pkg/state/impl/namespaced"
	akametadata "github.com/linode/go-metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	"github.com/siderolabs/talos/internal/app/machined/pkg/runtime"
	"github.com/siderolabs/talos/internal/app/machined/pkg/runtime/v1alpha1/platform/akamai"
	"github.com/siderolabs/talos/pkg/machinery/nethelpers"
	"github.com/siderolabs/talos/pkg/machinery/resources/network"
)

//go:embed testdata/instance-no-tags.json
var rawInstanceNoTags []byte

//go:embed testdata/instance-with-tags.json
var rawInstanceWithTags []byte

//go:embed testdata/network.json
var rawNetwork []byte

//go:embed testdata/network-dual-nic.json
var rawNetworkDualNIC []byte

//go:embed testdata/network-vpc-only.json
var rawNetworkVPCOnly []byte

//go:embed testdata/network-dual-nic-vpc-pending.json
var rawNetworkDualNICVPCPending []byte

//go:embed testdata/expected-no-tags.yaml
var expectedNoTags string

//go:embed testdata/expected-with-tags.yaml
var expectedWithTags string

//go:embed testdata/expected-dual-nic.yaml
var expectedDualNIC string

//go:embed testdata/expected-dual-nic-ens.yaml
var expectedDualNICens string

//go:embed testdata/expected-vpc-only.yaml
var expectedVPCOnly string

func TestParseMetadata(t *testing.T) {
	for _, tt := range []struct {
		name           string
		instance       []byte
		network        []byte
		linkNames      []string
		expected       string
		needsReconcile bool
	}{
		{
			name:      "no tags",
			instance:  rawInstanceNoTags,
			network:   rawNetwork,
			linkNames: []string{"eth0"},
			expected:  expectedNoTags,
		},
		{
			name:      "with tags",
			instance:  rawInstanceWithTags,
			network:   rawNetwork,
			linkNames: []string{"eth0"},
			expected:  expectedWithTags,
		},
		{
			name:      "dual-homed public and vpc",
			instance:  rawInstanceNoTags,
			network:   rawNetworkDualNIC,
			linkNames: []string{"eth0", "eth1"},
			expected:  expectedDualNIC,
		},
		{
			// Rename-invariance: the same metadata on a host that enumerates NICs
			// as ens3/ens4 must configure the VPC address on ens4, not a
			// non-existent eth1.
			name:      "dual-homed with predictable interface names",
			instance:  rawInstanceNoTags,
			network:   rawNetworkDualNIC,
			linkNames: []string{"ens3", "ens4"},
			expected:  expectedDualNICens,
		},
		{
			// A VPC-only node (no public interface, egress via 1:1 NAT) has the VPC
			// interface as its primary; it must keep its default DHCPv4 route.
			name:      "vpc-only node keeps its primary route",
			instance:  rawInstanceNoTags,
			network:   rawNetworkVPCOnly,
			linkNames: []string{"eth0"},
			expected:  expectedVPCOnly,
		},
		{
			// Links not enumerated yet: the VPC interface cannot be resolved, so it
			// is left unconfigured (no wrong-named spec) and a reconcile is
			// requested. Output degrades to the public-only config.
			name:           "links not enumerated yet requests reconcile",
			instance:       rawInstanceNoTags,
			network:        rawNetworkDualNIC,
			linkNames:      nil,
			expected:       expectedNoTags,
			needsReconcile: true,
		},
		{
			// The VPC interface's "vpc" object has not been populated by the
			// metadata service yet (a boot-time race): the links are enumerated but
			// the interface is left unconfigured this pass and a reconcile is
			// requested so a later fetch can pick up the VPC data. Output degrades
			// to the public-only config.
			name:           "vpc metadata not populated yet requests reconcile",
			instance:       rawInstanceNoTags,
			network:        rawNetworkDualNICVPCPending,
			linkNames:      []string{"eth0", "eth1"},
			expected:       expectedNoTags,
			needsReconcile: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := &akamai.Akamai{}

			var metadata akametadata.InstanceData

			var interfaceConfig akamai.NetworkData

			require.NoError(t, json.Unmarshal(tt.instance, &metadata))
			require.NoError(t, json.Unmarshal(tt.network, &interfaceConfig))

			networkConfig, needsReconcile, err := p.ParseMetadata(&metadata, &interfaceConfig, tt.linkNames)
			require.NoError(t, err)
			assert.Equal(t, tt.needsReconcile, needsReconcile)

			marshaled, err := yaml.Marshal(networkConfig)
			require.NoError(t, err)

			assert.Equal(t, tt.expected, string(marshaled))
		})
	}
}

func TestPhysicalLinkNames(t *testing.T) {
	mkLink := func(id, busPath, kind string) *network.LinkStatus {
		link := network.NewLinkStatus(network.NamespaceName, id)
		link.TypedSpec().Type = nethelpers.LinkEther
		link.TypedSpec().Kind = kind
		link.TypedSpec().BusPath = busPath

		return link
	}

	// Physical NICs out of order, interleaved with virtual links that must be
	// filtered out; the result is ordered by PCI bus path.
	links := []*network.LinkStatus{
		mkLink("ens4", "0000:00:04.0", ""),
		mkLink("cilium_host", "", "veth"),
		mkLink("ens3", "0000:00:03.0", ""),
		mkLink("bond0", "", "bond"),
	}

	assert.Equal(t, []string{"ens3", "ens4"}, akamai.PhysicalLinkNames(links))
}

func TestConvertTagsFromAkamai(t *testing.T) {
	for _, tt := range []struct {
		name     string
		input    []string
		expected map[string]string
	}{
		{
			name:     "nil input",
			input:    nil,
			expected: nil,
		},
		{
			name:     "empty slice",
			input:    []string{},
			expected: nil,
		},
		{
			name:  "single tag",
			input: []string{"tag1"},
			expected: map[string]string{
				"tag1": "",
			},
		},
		{
			name:  "multiple tags",
			input: []string{"tag1", "tag2", "tag3"},
			expected: map[string]string{
				"tag1": "",
				"tag2": "",
				"tag3": "",
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, akamai.ConvertTagsFromAkamai(tt.input))
		})
	}
}

func TestMetadataIncomplete(t *testing.T) {
	for _, tt := range []struct {
		name     string
		instance string
		network  string
		expected bool
	}{
		{
			name:     "empty instance data requests reconcile",
			instance: `{}`,
			network:  `{"interfaces":[],"ipv4":{"public":["172.1.2.3/32"]},"ipv6":{}}`,
			expected: true,
		},
		{
			name:     "empty network data requests reconcile",
			instance: `{"label":"talos"}`,
			network:  `{"interfaces":[],"ipv4":{"public":[]},"ipv6":{}}`,
			expected: true,
		},
		{
			name:     "single-nic node is complete",
			instance: `{"label":"talos"}`,
			network:  `{"interfaces":[],"ipv4":{"public":["172.1.2.3/32"]},"ipv6":{}}`,
			expected: false,
		},
		{
			name:     "vpc-only node is complete",
			instance: `{"label":"talos"}`,
			network:  string(rawNetworkVPCOnly),
			expected: false,
		},
		{
			name:     "dual-homed node is complete",
			instance: string(rawInstanceNoTags),
			network:  string(rawNetworkDualNIC),
			expected: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var instance akametadata.InstanceData

			var netData akamai.NetworkData

			require.NoError(t, json.Unmarshal([]byte(tt.instance), &instance))
			require.NoError(t, json.Unmarshal([]byte(tt.network), &netData))

			assert.Equal(t, tt.expected, akamai.MetadataIncomplete(&instance, &netData))
		})
	}
}

// TestReconcileNetworkConfigProgressiveMetadata simulates the Akamai metadata
// service publishing data in stages at boot: (0) empty, (1) instance + public +
// interfaces but the VPC interface's "vpc" object still nil, (2) fully populated.
// The driver must re-fetch and retry until stage 2 and then configure the VPC
// address on eth1, rather than caching an early, incomplete snapshot.
func TestReconcileNetworkConfigProgressiveMetadata(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)

	st := state.WrapCore(namespaced.NewState(inmem.Build))

	// Two physical NICs in PCI order: eth0 (public), eth1 (VPC).
	for _, l := range []struct{ id, bus string }{{"eth0", "0000:00:03.0"}, {"eth1", "0000:00:04.0"}} {
		link := network.NewLinkStatus(network.NamespaceName, l.id)
		link.TypedSpec().Type = nethelpers.LinkEther
		link.TypedSpec().BusPath = l.bus
		require.NoError(t, st.Create(ctx, link))
	}

	var emptyInstance, fullInstance akametadata.InstanceData

	require.NoError(t, json.Unmarshal([]byte(`{}`), &emptyInstance))
	require.NoError(t, json.Unmarshal(rawInstanceNoTags, &fullInstance))

	var emptyNet, pendingNet, fullNet akamai.NetworkData

	require.NoError(t, json.Unmarshal([]byte(`{"interfaces":[],"ipv4":{"public":[]},"ipv6":{}}`), &emptyNet))
	require.NoError(t, json.Unmarshal(rawNetworkDualNICVPCPending, &pendingNet))
	require.NoError(t, json.Unmarshal(rawNetworkDualNIC, &fullNet))

	stages := []struct {
		instance *akametadata.InstanceData
		network  *akamai.NetworkData
	}{
		{&emptyInstance, &emptyNet},
		{&fullInstance, &pendingNet},
		{&fullInstance, &fullNet},
	}

	call := 0
	fetch := func(context.Context) (*akametadata.InstanceData, *akamai.NetworkData, error) {
		idx := call
		if idx >= len(stages) {
			idx = len(stages) - 1
		}

		call++

		return stages[idx].instance, stages[idx].network, nil
	}

	p := &akamai.Akamai{}
	ch := make(chan *runtime.PlatformNetworkConfig, len(stages)+2)
	errCh := make(chan error, 1)

	go func() {
		errCh <- p.ReconcileNetworkConfig(ctx, st, ch, fetch)
	}()

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-ctx.Done():
		require.FailNow(t, "timed out waiting for reconcile to complete")
	}

	close(ch)

	var last *runtime.PlatformNetworkConfig

	for cfg := range ch {
		last = cfg
	}

	// It progressed through all three stages...
	assert.GreaterOrEqual(t, call, 3)

	// ...and the final config configures the VPC address on eth1 (the fix).
	require.NotNil(t, last)

	var eth1 *network.AddressSpecSpec

	for i := range last.Addresses {
		if last.Addresses[i].LinkName == "eth1" {
			eth1 = &last.Addresses[i]
		}
	}

	require.NotNil(t, eth1, "expected a platform-layer address on eth1")
	assert.Equal(t, "10.20.0.10/24", eth1.Address.String())
	assert.Equal(t, network.ConfigPlatform, eth1.ConfigLayer)
}
