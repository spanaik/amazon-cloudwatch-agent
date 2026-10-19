// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

// Package statestore keeps one versioned record for a component in an OpenTelemetry storage
// extension (for example file_storage), so the component can restore what it knew after an agent
// restart.
//
// The record is stored under Options.Key as a JSON envelope:
//
//	{"version": <int>, "data": <T>}
//
// Load discards a record that does not decode or whose version differs from Options.Version, so a
// schema change only needs a version bump. Writers call MarkChanged when the state changes and
// MarkTouched when it is still current but worth refreshing; one saver goroutine per Store coalesces
// the marks into at most one write per Debounce interval (MarkChanged) or per IdleFlush interval
// (MarkTouched only), and Shutdown writes whatever is still pending.
package statestore
