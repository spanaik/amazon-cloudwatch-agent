// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package statestore

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/extension/xextension/storage"
)

// GetClient finds the storage extension storageID in host and returns a client of that extension for
// the component with the given kind and id. It returns an error when storageID is nil, when host has
// no extension with that ID, and when that extension is not a storage extension.
func GetClient(ctx context.Context, host component.Host, storageID *component.ID, kind component.Kind, id component.ID) (storage.Client, error) {
	if storageID == nil {
		return nil, errors.New("statestore: no storage extension ID")
	}
	ext, found := host.GetExtensions()[*storageID]
	if !found {
		return nil, fmt.Errorf("statestore: storage extension %q is not configured", storageID.String())
	}
	storageExt, isStorage := ext.(storage.Extension)
	if !isStorage {
		return nil, fmt.Errorf("statestore: extension %q is not a storage extension", storageID.String())
	}
	// Remove the underscores of the type, so a rename that adds underscores keeps the same storage (https://github.com/open-telemetry/opentelemetry-collector/issues/14988).
	clientID := component.MustNewIDWithName(strings.ReplaceAll(id.Type().String(), "_", ""), id.Name())
	return storageExt.GetClient(ctx, kind, clientID, "")
}
