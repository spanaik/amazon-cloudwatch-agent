// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package vllmconfig

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const secretValue = "s3cr3t-value-never-cached"

func testPod(name, uid, ip string, port int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "inference", UID: types.UID(uid),
			Annotations: map[string]string{"note": secretValue},
			Labels:      map[string]string{"app": "vllm"},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-a",
			Containers: []corev1.Container{
				{Name: "sidecar", Image: "busybox", Command: []string{"sleep", "inf"}},
				{
					Name:    "vllm",
					Image:   "vllm/vllm-openai:v0.11.2",
					Command: []string{"vllm", "serve"},
					Args: []string{"Qwen/Qwen2.5-0.5B-Instruct", "--tensor-parallel-size", "2",
						"--api-key", "$(API_KEY)", "--max-model-len", "$(MAX_LEN)"},
					Env: []corev1.EnvVar{
						{Name: "API_KEY", Value: secretValue},
						{Name: "MAX_LEN", Value: "4096"},
						{Name: "MAX_MODEL_LEN", Value: "999"},
					},
					Ports: []corev1.ContainerPort{{ContainerPort: port}, {ContainerPort: 9000, Protocol: corev1.ProtocolUDP}},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning, PodIP: ip, HostIP: "192.168.0.1",
			ContainerStatuses: []corev1.ContainerStatus{{Name: "vllm", RestartCount: 2}},
		},
	}
}

func startWatcher(t *testing.T, client kubernetes.Interface, envMap map[string]string) *podWatcher {
	w, err := newPodWatcher(client, "node-a", envMap, zap.NewNop(), nil)
	require.NoError(t, err)
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	w.start(stop)
	return w
}

func TestPodWatcherLookupAndMinimize(t *testing.T) {
	client := fake.NewClientset(testPod("vllm-0", "uid-a", "10.0.0.5", 8000))
	w := startWatcher(t, client, map[string]string{"MAX_MODEL_LEN": valMaxModelLen})

	// the watch is limited to this node's pods
	var selectors []string
	for _, a := range client.Actions() {
		if la, ok := a.(k8stesting.ListAction); ok {
			selectors = append(selectors, la.GetListRestrictions().Fields.String())
		}
	}
	assert.Contains(t, selectors, "spec.nodeName=node-a")

	v, ok := w.lookup(scrapeTarget{host: "10.0.0.5", port: "8000"})
	require.True(t, ok)
	assert.Equal(t, types.UID("uid-a"), v.uid)
	assert.Equal(t, int64(2), v.restarts)
	c, ok := v.scrapedContainer(scrapeTarget{port: "8000"})
	require.True(t, ok)
	assert.Equal(t, "vllm", c.Name)
	assert.Equal(t, []string{"8000"}, c.Ports) // TCP ports only
	assert.Equal(t, fieldValue{Status: statusSet, Value: "2"}, c.Args[valTensorParallelSize])
	assert.Equal(t, fieldValue{Status: statusSet, Value: "4096"}, c.Args[valMaxModelLen])
	assert.Equal(t, fieldValue{Status: statusSet, Value: "999"}, c.Env[valMaxModelLen])

	// the cached object has no command, args, env, annotations or labels of the pod,
	// and no raw value anywhere
	objs := w.informer.GetIndexer().List()
	require.Len(t, objs, 1)
	cached := objs[0].(*corev1.Pod)
	assert.Empty(t, cached.Spec.Containers)
	assert.Empty(t, cached.Labels)
	assert.Len(t, cached.Annotations, 1)
	raw, err := json.Marshal(cached)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), secretValue)
	assert.NotContains(t, string(raw), "--api-key")

	// unknown IP, then fallback by namespace and name
	_, ok = w.lookup(scrapeTarget{host: "10.9.9.9", port: "8000"})
	assert.False(t, ok)
	_, ok = w.lookup(scrapeTarget{host: "10.9.9.9", namespace: "inference", podName: "vllm-0"})
	assert.True(t, ok)
	// the receiver's pod UID must match
	_, ok = w.lookup(scrapeTarget{host: "10.0.0.5", podUID: "other"})
	assert.False(t, ok)
}

func TestMinimizePodIgnoresForgedAnnotation(t *testing.T) {
	pod := testPod("p", "u", "10.0.0.1", 8000)
	pod.Annotations[podInfoAnnotation] = `{"c":[{"n":"vllm","a":{"dtype":{"s":2,"v":"forged"}}}]}`
	out, err := minimizePod(nil)(pod)
	require.NoError(t, err)
	info := decodePodInfo(out.(*corev1.Pod))
	require.Len(t, info.Containers, 2)
	assert.Equal(t, statusDefault, info.Containers[1].Args[valDtype].Status)

	// a pod object from the API server is never trusted as minimized
	assert.Empty(t, decodePodInfo(pod).Containers)
	// the transform is idempotent
	again, err := minimizePod(nil)(out)
	require.NoError(t, err)
	assert.Same(t, out, again)
}

func TestChoosePodHostNetwork(t *testing.T) {
	a, _ := minimizePod(nil)(testPod("a", "ua", "192.168.0.1", 8000))
	b, _ := minimizePod(nil)(testPod("b", "ub", "192.168.0.1", 8001))
	pods := []*corev1.Pod{a.(*corev1.Pod), b.(*corev1.Pod)}
	assert.Equal(t, types.UID("ub"), choosePod(pods, scrapeTarget{port: "8001"}).UID)
	assert.Nil(t, choosePod(pods, scrapeTarget{port: "7777"}))
}

// TestProcessorEndToEnd runs the processor with a fake clientset and a fake vLLM API.
func TestProcessorEndToEnd(t *testing.T) {
	api := newFakeServer(t, map[string]func(http.ResponseWriter){
		"/version":   body(200, `{"version":"0.11.2"}`),
		"/v1/models": body(200, openAIModels),
	})
	host, port := api.hostPort(t)
	pod := testPod("vllm-0", "uid-e2e", host, 0)
	pod.Spec.Containers[1].Ports[0].ContainerPort = int32(mustAtoi(t, port))
	client := fake.NewClientset(pod)

	cfg := NewFactory().CreateDefaultConfig().(*Config)
	cfg.NodeName = "node-a"
	p := newVLLMConfigProcessor(cfg, zap.NewNop())
	p.newClient = func() (kubernetes.Interface, error) { return client, nil }
	require.NoError(t, p.start(context.Background(), componenttest.NewNopHost()))
	defer func() { require.NoError(t, p.shutdown(context.Background())) }()

	md := scrape(host, port, histogram("vllm:time_to_first_token_seconds"), gauge("vllm:num_requests_running", nil))
	start := time.Now()
	out := process(t, p, md)
	assert.Less(t, time.Since(start), cfg.Timeout)
	res := out.ResourceMetrics().At(0).Resource().Attributes().AsRaw()
	assert.Equal(t, "0.11.2", res["vllm.config.version"])
	assert.Equal(t, "Qwen/Qwen2.5-0.5B-Instruct", res["vllm.config.model"])
	assert.Equal(t, "V1", res["vllm.config.engine"])
	assert.Equal(t, "2", dpAttrs(t, out, "vllm:time_to_first_token_seconds")["vllm.config.tensor_parallel_size"])
	assert.Equal(t, "2048", dpAttrs(t, out, "vllm:num_requests_running")["vllm.config.max_model_len"])
	for _, r := range api.reqs() {
		assert.Empty(t, r.header.Get("Authorization"))
		assert.True(t, strings.HasPrefix(r.path, "/v"))
	}
}

func mustAtoi(t *testing.T, s string) int {
	var n int
	for _, c := range s {
		require.True(t, c >= '0' && c <= '9')
		n = n*10 + int(c-'0')
	}
	return n
}
