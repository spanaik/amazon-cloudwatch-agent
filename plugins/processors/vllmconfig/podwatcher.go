// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package vllmconfig

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

const (
	// minimizedMarker marks a pod object that the transform built. The API server
	// never sends this apiVersion, so a customer annotation cannot pose as one.
	minimizedMarker = "vllmconfig.cloudwatch-agent.internal/v1"
	// podInfoAnnotation holds the parsed, allowlisted values of the pod.
	podInfoAnnotation = "vllmconfig.cloudwatch-agent.internal/info"
	ipIndex           = "podIP"
	syncWait          = 5 * time.Second
)

// containerInfo is what the transform keeps of a container: its TCP ports and the
// parsed values. Raw command, args and env values are not kept.
type containerInfo struct {
	Name  string                `json:"n"`
	Ports []string              `json:"p,omitempty"`
	VLLM  bool                  `json:"m,omitempty"` // the image or command line names vLLM
	Args  map[string]fieldValue `json:"a,omitempty"` // nil: no vLLM command line in the pod spec
	Env   envContract           `json:"e,omitempty"`
	UseV1 string                `json:"v1,omitempty"`
}

type podInfo struct {
	Containers []containerInfo `json:"c"`
}

// podView is one pod as the resolver sees it.
type podView struct {
	uid       types.UID
	name      string
	namespace string
	restarts  int64
	info      podInfo
}

// podLookup finds the pod of a scrape target.
type podLookup interface {
	lookup(t scrapeTarget) (podView, bool)
}

// podInfoFromSpec parses the allowlisted values out of the pod spec.
func podInfoFromSpec(pod *corev1.Pod, envMap map[string]string) podInfo {
	var info podInfo
	for _, c := range pod.Spec.Containers {
		spec := containerSpec{Command: c.Command, Args: c.Args, EnvFrom: len(c.EnvFrom) > 0, Image: c.Image}
		for _, e := range c.Env {
			spec.Env = append(spec.Env, envVar{Name: e.Name, Value: e.Value, FromSource: e.ValueFrom != nil})
		}
		ci := containerInfo{Name: c.Name, VLLM: mentionsVLLM(spec)}
		for _, p := range c.Ports {
			if p.Protocol == "" || p.Protocol == corev1.ProtocolTCP {
				ci.Ports = append(ci.Ports, strconv.Itoa(int(p.ContainerPort)))
			}
		}
		if res := parseContainerArgs(spec); res.found {
			ci.Args = res.fields
		}
		ci.Env = readEnvContract(envMap, spec.Env, spec.EnvFrom)
		ci.UseV1 = useV1Env(spec.Env, spec.EnvFrom)
		info.Containers = append(info.Containers, ci)
	}
	return info
}

// minimizePod is the informer transform. It parses the allowlisted values and
// returns a small pod object without command, args, env, annotations or labels.
func minimizePod(envMap map[string]string) cache.TransformFunc {
	return func(obj any) (any, error) {
		pod, ok := obj.(*corev1.Pod)
		if !ok || pod.APIVersion == minimizedMarker {
			return obj, nil
		}
		data, err := json.Marshal(podInfoFromSpec(pod, envMap))
		if err != nil {
			data = []byte(`{"c":null}`)
		}
		out := &corev1.Pod{
			TypeMeta: metav1.TypeMeta{APIVersion: minimizedMarker},
			ObjectMeta: metav1.ObjectMeta{
				Name:            pod.Name,
				Namespace:       pod.Namespace,
				UID:             pod.UID,
				ResourceVersion: pod.ResourceVersion,
				Annotations:     map[string]string{podInfoAnnotation: string(data)},
			},
			Spec: corev1.PodSpec{NodeName: pod.Spec.NodeName, HostNetwork: pod.Spec.HostNetwork},
			Status: corev1.PodStatus{
				Phase:  pod.Status.Phase,
				HostIP: pod.Status.HostIP,
				PodIP:  pod.Status.PodIP,
				PodIPs: pod.Status.PodIPs,
			},
		}
		for _, cs := range pod.Status.ContainerStatuses {
			out.Status.ContainerStatuses = append(out.Status.ContainerStatuses,
				corev1.ContainerStatus{Name: cs.Name, RestartCount: cs.RestartCount})
		}
		return out, nil
	}
}

func indexByIP(obj any) ([]string, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil, nil
	}
	var ips []string
	if pod.Status.PodIP != "" {
		ips = append(ips, pod.Status.PodIP)
	}
	for _, ip := range pod.Status.PodIPs {
		if ip.IP != "" && ip.IP != pod.Status.PodIP {
			ips = append(ips, ip.IP)
		}
	}
	return ips, nil
}

// podWatcher watches the pods of one node.
type podWatcher struct {
	logger   *zap.Logger
	informer cache.SharedIndexInformer
	factory  informers.SharedInformerFactory
	onDelete func(types.UID)

	warnMu   sync.Mutex
	lastWarn time.Time
}

func newPodWatcher(client kubernetes.Interface, nodeName string, envMap map[string]string, logger *zap.Logger, onDelete func(types.UID)) (*podWatcher, error) {
	factory := informers.NewSharedInformerFactoryWithOptions(client, 0,
		informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.FieldSelector = fields.OneTermEqualSelector("spec.nodeName", nodeName).String()
		}))
	informer := factory.Core().V1().Pods().Informer()
	w := &podWatcher{logger: logger, informer: informer, factory: factory, onDelete: onDelete}
	if err := informer.SetTransform(minimizePod(envMap)); err != nil {
		return nil, err
	}
	if err := informer.AddIndexers(cache.Indexers{ipIndex: indexByIP}); err != nil {
		return nil, err
	}
	if err := informer.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
		w.warnMu.Lock()
		defer w.warnMu.Unlock()
		if time.Since(w.lastWarn) >= time.Minute {
			w.lastWarn = time.Now()
			logger.Warn("vllmconfig: pod watch failed; metrics pass through without engine values until it recovers", zap.Error(err))
		}
	}); err != nil {
		return nil, err
	}
	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: func(obj any) {
			if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = d.Obj
			}
			if pod, ok := obj.(*corev1.Pod); ok && w.onDelete != nil {
				w.onDelete(pod.UID)
			}
		},
	}); err != nil {
		return nil, err
	}
	return w, nil
}

// start runs the informer and waits a short time for the first list.
func (w *podWatcher) start(stopCh <-chan struct{}) {
	w.factory.Start(stopCh)
	ctx, cancel := context.WithTimeout(context.Background(), syncWait)
	defer cancel()
	go func() {
		select {
		case <-stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	if !cache.WaitForCacheSync(ctx.Done(), w.informer.HasSynced) {
		w.logger.Warn("vllmconfig: pod informer has not synced yet; metrics pass through until it does")
	}
}

func (w *podWatcher) lookup(t scrapeTarget) (podView, bool) {
	var candidates []*corev1.Pod
	if t.host != "" {
		objs, _ := w.informer.GetIndexer().ByIndex(ipIndex, t.host)
		for _, o := range objs {
			if pod, ok := o.(*corev1.Pod); ok && pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
				candidates = append(candidates, pod)
			}
		}
	}
	if len(candidates) == 0 && t.namespace != "" && t.podName != "" {
		if o, exists, _ := w.informer.GetIndexer().GetByKey(t.namespace + "/" + t.podName); exists {
			if pod, ok := o.(*corev1.Pod); ok {
				candidates = append(candidates, pod)
			}
		}
	}
	pod := choosePod(candidates, t)
	if pod == nil {
		return podView{}, false
	}
	return viewOf(pod), true
}

// choosePod picks the scraped pod: by UID when the receiver gives it, else the only
// candidate, else the one pod whose containers declare the scrape port. Pods with
// host networking share an IP, so an ambiguous match gives no pod.
func choosePod(candidates []*corev1.Pod, t scrapeTarget) *corev1.Pod {
	if t.podUID != "" {
		for _, p := range candidates {
			if string(p.UID) == t.podUID {
				return p
			}
		}
		return nil
	}
	if len(candidates) == 1 {
		return candidates[0]
	}
	var match *corev1.Pod
	for _, p := range candidates {
		info := decodePodInfo(p)
		for _, c := range info.Containers {
			for _, port := range c.Ports {
				if port == t.port {
					if match != nil && match != p {
						return nil
					}
					match = p
				}
			}
		}
	}
	return match
}

func decodePodInfo(pod *corev1.Pod) podInfo {
	var info podInfo
	if pod.APIVersion != minimizedMarker {
		return info
	}
	_ = json.Unmarshal([]byte(pod.Annotations[podInfoAnnotation]), &info)
	return info
}

func viewOf(pod *corev1.Pod) podView {
	v := podView{uid: pod.UID, name: pod.Name, namespace: pod.Namespace, info: decodePodInfo(pod)}
	for _, cs := range pod.Status.ContainerStatuses {
		v.restarts += int64(cs.RestartCount)
	}
	return v
}

// scrapedContainer picks the container that serves the scrape: the one that
// declares the scrape port, else the one the receiver names, else the first one
// whose image or command line names vLLM, else the first one.
func (v podView) scrapedContainer(t scrapeTarget) (containerInfo, bool) {
	cs := v.info.Containers
	if len(cs) == 0 {
		return containerInfo{}, false
	}
	for _, c := range cs {
		for _, p := range c.Ports {
			if p == t.port {
				return c, true
			}
		}
	}
	if t.container != "" {
		for _, c := range cs {
			if c.Name == t.container {
				return c, true
			}
		}
	}
	for _, c := range cs {
		if c.VLLM {
			return c, true
		}
	}
	return cs[0], true
}

// probePorts gives the scrape port first, then the other TCP ports of the
// container, at most maxProbePorts in total.
func probePorts(t scrapeTarget, c containerInfo) []string {
	var ports []string
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !seen[p] && len(ports) < maxProbePorts {
			seen[p] = true
			ports = append(ports, p)
		}
	}
	add(t.port)
	for _, p := range c.Ports {
		add(p)
	}
	return ports
}
