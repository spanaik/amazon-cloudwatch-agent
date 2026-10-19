// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package signaltoinfometricconnector

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

func TestBuildMetrics(t *testing.T) {
	cfg := testConfig()
	cfg.Rules[0].Identity = []string{"k8s.pod.uid", "k8s.container.name"}
	cfg.Rules[0].IncludeResourceAttributes = []string{"k8s.pod.name", "k8s.namespace.name", "k8s.pod.uid", "k8s.container.name"}
	second := cfg.Rules[0]
	second.Name = "other_info"
	second.Description = ""
	second.Unit = ""
	cfg.Rules = append(cfg.Rules, second)

	first := base.UnixNano()
	entries := []emitEntry{
		{rule: 0, entity: entity{
			Identity:  []kv{{"k8s.pod.uid", "u1"}, {"k8s.container.name", "c1"}},
			Resource:  []kv{{"k8s.pod.name", "p1"}, {"k8s.namespace.name", "ns"}, {"k8s.pod.uid", "u1"}, {"k8s.container.name", "c1"}},
			Facts:     []kv{{"z", "1"}, {"a", "2"}, {"m", "3"}},
			FirstSeen: first,
			Updated:   first + 10,
			Activity:  first + 20,
		}},
		// A resource attribute was absent, and a FirstSeen after the emit
		// time (clock skew) is clamped.
		{rule: 1, entity: entity{
			Identity:  []kv{{"k8s.pod.uid", "u2"}, {"k8s.container.name", "c2"}},
			Resource:  []kv{{"k8s.pod.name", "p2"}},
			Facts:     []kv{{"z", "9"}},
			FirstSeen: testNow.Add(time.Hour).UnixNano(),
		}},
	}

	for _, tc := range []struct {
		name         string
		prefix       string
		wantResource [][]kv
	}{
		{
			name:   "identity_output_prefix",
			prefix: "id.",
			wantResource: [][]kv{
				{{"k8s.pod.name", "p1"}, {"k8s.namespace.name", "ns"}, {"k8s.pod.uid", "u1"}, {"k8s.container.name", "c1"}, {"id.k8s.pod.uid", "u1"}, {"id.k8s.container.name", "c1"}},
				{{"k8s.pod.name", "p2"}, {"id.k8s.pod.uid", "u2"}, {"id.k8s.container.name", "c2"}},
			},
		},
		{
			name: "no_identity_output",
			wantResource: [][]kv{
				{{"k8s.pod.name", "p1"}, {"k8s.namespace.name", "ns"}, {"k8s.pod.uid", "u1"}, {"k8s.container.name", "c1"}},
				{{"k8s.pod.name", "p2"}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg.IdentityOutputPrefix = tc.prefix
			conn, _ := newTestConnector(t, cfg, consumertest.NewNop())
			md := conn.buildMetrics(testNow, entries)
			got := allSeries(t, []pmetric.Metrics{md})
			require.Len(t, got, 2)
			assert.Equal(t, series{
				resource:       tc.wantResource[0],
				name:           "app_info",
				start:          base,
				ts:             testNow,
				attrs:          []kv{{"z", "1"}, {"a", "2"}, {"m", "3"}},
				value:          1,
				scopeName:      "github.com/aws/amazon-cloudwatch-agent/connector/signaltoinfometricconnector",
				description:    "App configuration.",
				unit:           "1",
				dataPointCount: 2,
			}, got[0])
			assert.Equal(t, series{
				resource:       tc.wantResource[1],
				name:           "other_info",
				start:          testNow,
				ts:             testNow,
				attrs:          []kv{{"z", "9"}},
				value:          1,
				scopeName:      scopeName,
				dataPointCount: 2,
			}, got[1])
		})
	}

	conn, _ := newTestConnector(t, testConfig(), consumertest.NewNop())
	assert.Equal(t, 0, conn.buildMetrics(testNow, nil).ResourceMetrics().Len())
}

func TestEmitSkipsEmptyEntries(t *testing.T) {
	next := &countingConsumer{}
	conn, _ := newTestConnector(t, testConfig(), next)
	conn.emit(t.Context(), nil)
	assert.Equal(t, 0, next.calls())
}
