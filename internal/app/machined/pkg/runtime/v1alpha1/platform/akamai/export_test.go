// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package akamai

import (
	"context"

	"github.com/cosi-project/runtime/pkg/state"

	"github.com/siderolabs/talos/internal/app/machined/pkg/runtime"
)

var (
	ConvertTagsFromAkamai = convertTagsFromAkamai
	PhysicalLinkNames     = physicalLinkNames
	MetadataIncomplete    = metadataIncomplete
)

// MetadataFetcher is the injectable metadata fetch used by ReconcileNetworkConfig.
type MetadataFetcher = metadataFetcher

// ReconcileNetworkConfig exposes reconcileNetworkConfig to tests with an
// injectable metadata fetcher.
func (a *Akamai) ReconcileNetworkConfig(ctx context.Context, st state.State, ch chan<- *runtime.PlatformNetworkConfig, fetch MetadataFetcher) error {
	return a.reconcileNetworkConfig(ctx, st, ch, fetch)
}
