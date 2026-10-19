// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

// GetClient is based on GetStorageClient in
// https://github.com/open-telemetry/opentelemetry-collector-contrib/blob/v0.150.0/pkg/stanza/adapter/storage.go
// and takes the component kind as a parameter instead of always using component.KindReceiver.

// Copyright The OpenTelemetry Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package statestore

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/extension/xextension/storage"
)

// GetClient returns the storage client the component identified by kind and id should use. A nil
// storageID returns storage.NewNopClient(), so a component without persistence configured needs no
// special case.
func GetClient(ctx context.Context, host component.Host, storageID *component.ID, kind component.Kind, id component.ID) (storage.Client, error) {
	if storageID == nil {
		return storage.NewNopClient(), nil
	}

	ext, ok := host.GetExtensions()[*storageID]
	if !ok {
		return nil, fmt.Errorf("storage extension '%s' not found", storageID)
	}

	storageExt, ok := ext.(storage.Extension)
	if !ok {
		return nil, fmt.Errorf("non-storage extension '%s' found", storageID)
	}

	// Make storage immune to component renames that add underscores to the component type.
	// This is a workaround for https://github.com/open-telemetry/opentelemetry-collector/issues/14988.
	normalizedComponentType := strings.ReplaceAll(id.Type().String(), "_", "")
	normalizedComponentID := component.MustNewIDWithName(normalizedComponentType, id.Name())
	return storageExt.GetClient(ctx, kind, normalizedComponentID, "")
}
