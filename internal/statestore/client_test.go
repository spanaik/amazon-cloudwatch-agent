// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package statestore

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/open-telemetry/opentelemetry-collector-contrib/extension/storage/filestorage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/extension"
	"go.opentelemetry.io/collector/extension/extensiontest"
	"go.opentelemetry.io/collector/extension/xextension/storage"
	"go.uber.org/zap"
)

type testHost map[component.ID]component.Component

func (h testHost) GetExtensions() map[component.ID]component.Component {
	return h
}

// recordingStorage is a storage.Extension that records what GetClient was asked for.
type recordingStorage struct {
	component.StartFunc
	component.ShutdownFunc
	kind component.Kind
	id   component.ID
	name string
}

func (r *recordingStorage) GetClient(_ context.Context, kind component.Kind, id component.ID, name string) (storage.Client, error) {
	r.kind, r.id, r.name = kind, id, name
	return newFakeClient(), nil
}

var _ storage.Extension = (*recordingStorage)(nil)

func TestGetClientNilID(t *testing.T) {
	client, err := GetClient(context.Background(), testHost{}, nil, component.KindConnector, component.MustNewID("my_connector"))
	require.NoError(t, err)
	assert.Equal(t, storage.NewNopClient(), client)
}

func TestGetClientMissingExtension(t *testing.T) {
	storageID := component.MustNewIDWithName("file_storage", "missing")
	_, err := GetClient(context.Background(), testHost{}, &storageID, component.KindConnector, component.MustNewID("my_connector"))
	assert.EqualError(t, err, "storage extension 'file_storage/missing' not found")
}

func TestGetClientNonStorageExtension(t *testing.T) {
	nopFactory := extensiontest.NewNopFactory()
	nop, err := nopFactory.Create(context.Background(), extensiontest.NewNopSettings(nopFactory.Type()), nopFactory.CreateDefaultConfig())
	require.NoError(t, err)
	storageID := component.MustNewID("nop")

	_, err = GetClient(context.Background(), testHost{storageID: nop}, &storageID, component.KindConnector, component.MustNewID("my_connector"))
	assert.EqualError(t, err, "non-storage extension 'nop' found")
}

func TestGetClientPassesKindAndNormalizedID(t *testing.T) {
	ext := &recordingStorage{}
	storageID := component.MustNewID("file_storage")

	client, err := GetClient(context.Background(), testHost{storageID: ext}, &storageID,
		component.KindConnector, component.MustNewIDWithName("signal_to_info_metric", "pods"))
	require.NoError(t, err)
	assert.NotNil(t, client)
	assert.Equal(t, component.KindConnector, ext.kind)
	assert.Equal(t, component.MustNewIDWithName("signaltoinfometric", "pods"), ext.id)
	assert.Empty(t, ext.name)
}

func startFileStorage(t *testing.T, dir string) extension.Extension {
	t.Helper()
	f := filestorage.NewFactory()
	cfg := f.CreateDefaultConfig().(*filestorage.Config)
	cfg.Directory = dir
	ext, err := f.Create(context.Background(), extensiontest.NewNopSettings(f.Type()), cfg)
	require.NoError(t, err)
	require.NoError(t, ext.Start(context.Background(), componenttest.NewNopHost()))
	return ext
}

// TestFileStorageRoundTrip writes through a real file_storage extension, restarts the extension on
// the same directory, and reads the record back, as happens across an agent restart.
func TestFileStorageRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	storageID := component.MustNewID("file_storage")
	connectorID := component.MustNewIDWithName("signal_to_info_metric", "pods")
	opts := Options{Key: "pods", Version: 1, MaxBytes: 1 << 20, Logger: zap.NewNop()}
	want := record{Pods: map[string]string{"ns/pod-a": "uid-1"}}

	ext := startFileStorage(t, dir)
	client, err := GetClient(ctx, testHost{storageID: ext}, &storageID, component.KindConnector, connectorID)
	require.NoError(t, err)
	writer, err := New[record](client, opts)
	require.NoError(t, err)
	_, found, err := writer.Load(ctx)
	require.NoError(t, err)
	require.False(t, found)
	writer.Start(constSnapshot(want))
	writer.MarkChanged()
	require.NoError(t, writer.Shutdown(ctx), "Shutdown saves the pending change and closes the client")
	require.NoError(t, ext.Shutdown(ctx))
	// The file name shows the extension received the connector kind and the normalized ID.
	assert.FileExists(t, filepath.Join(dir, "connector_signaltoinfometric_pods"))

	ext = startFileStorage(t, dir)
	client, err = GetClient(ctx, testHost{storageID: ext}, &storageID, component.KindConnector, connectorID)
	require.NoError(t, err)
	reader, err := New[record](client, opts)
	require.NoError(t, err)
	got, found, err := reader.Load(ctx)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, want, got)
	require.NoError(t, reader.Shutdown(ctx))
	require.NoError(t, ext.Shutdown(ctx))
}
