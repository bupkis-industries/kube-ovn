package daemon

import (
	"context"
	"fmt"
	"strings"

	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
)

// enqueueUpdateIPForReIP fires when an IP CR mutates v4/v6/MAC on the local
// node — the controller has already swapped the LSP and we need to refresh the
// pod's netns to match. Filters out IPs that don't belong to this node and
// IPs whose addressing didn't change (label-only updates etc.).
func (c *Controller) enqueueUpdateIPForReIP(oldObj, newObj any) {
	oldIP, ok := oldObj.(*kubeovnv1.IP)
	if !ok {
		return
	}
	newIP, ok := newObj.(*kubeovnv1.IP)
	if !ok {
		return
	}
	if newIP.Spec.NodeName != c.config.NodeName {
		return
	}
	if oldIP.Spec.V4IPAddress == newIP.Spec.V4IPAddress &&
		oldIP.Spec.V6IPAddress == newIP.Spec.V6IPAddress &&
		oldIP.Spec.MacAddress == newIP.Spec.MacAddress {
		return
	}
	if newIP.Spec.PodName == "" || newIP.Spec.Namespace == "" {
		// non-pod IPs (node, u2o, mcast querier) — out of scope for live re-IP.
		return
	}
	klog.Infof("enqueue re-IP for IP CR %s (pod %s/%s) v4 %s->%s v6 %s->%s",
		newIP.Name, newIP.Spec.Namespace, newIP.Spec.PodName,
		oldIP.Spec.V4IPAddress, newIP.Spec.V4IPAddress,
		oldIP.Spec.V6IPAddress, newIP.Spec.V6IPAddress)
	c.reIPQueue.Add(newIP.Name)
}

func (c *Controller) runReIPWorker() {
	for c.processNextReIPWorkItem() {
	}
}

func (c *Controller) processNextReIPWorkItem() bool {
	key, shutdown := c.reIPQueue.Get()
	if shutdown {
		return false
	}
	err := func(key string) error {
		defer c.reIPQueue.Done(key)
		if err := c.handlePodReIP(key); err != nil {
			c.reIPQueue.AddRateLimited(key)
			return fmt.Errorf("re-IP %s: %w", key, err)
		}
		c.reIPQueue.Forget(key)
		return nil
	}(key)
	if err != nil {
		utilruntime.HandleError(err)
	}
	return true
}

func (c *Controller) handlePodReIP(key string) error {
	ipCR, err := c.ipsLister.Get(key)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if ipCR.Spec.NodeName != c.config.NodeName {
		return nil
	}
	pod, err := c.podsLister.Pods(ipCR.Spec.Namespace).Get(ipCR.Spec.PodName)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if pod.DeletionTimestamp != nil {
		return nil
	}
	subnet, err := c.subnetsLister.Get(ipCR.Spec.Subnet)
	if err != nil {
		return fmt.Errorf("get subnet %s: %w", ipCR.Spec.Subnet, err)
	}

	newIPStr := strings.TrimSuffix(strings.Join(nonEmpty(ipCR.Spec.V4IPAddress, ipCR.Spec.V6IPAddress), ","), ",")
	if newIPStr == "" {
		return nil
	}

	if err := c.reassignPodNetns(pod, subnet, newIPStr, ipCR.Spec.MacAddress); err != nil {
		return err
	}
	return c.patchPodStatusIPs(pod, ipCR)
}

func nonEmpty(ss ...string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// patchPodStatusIPs updates pod.status.podIP/podIPs to reflect the new
// addresses. Without this, EndpointSlice reconciliation, kube-proxy, downward
// API consumers, and `kubectl get pod -o wide` all see stale IPs. The daemon
// has node-scoped pod-status patch permission today (used by kubelet); kube-ovn
// needs it for live re-IP.
func (c *Controller) patchPodStatusIPs(pod *v1.Pod, ipCR *kubeovnv1.IP) error {
	primary := ""
	switch {
	case ipCR.Spec.V4IPAddress != "" && ipCR.Spec.V6IPAddress != "":
		primary = ipCR.Spec.V4IPAddress
	case ipCR.Spec.V4IPAddress != "":
		primary = ipCR.Spec.V4IPAddress
	case ipCR.Spec.V6IPAddress != "":
		primary = ipCR.Spec.V6IPAddress
	}
	if primary == "" {
		return nil
	}
	if pod.Status.PodIP == primary {
		return nil
	}
	patch := map[string]any{
		"status": map[string]any{
			"podIP":  primary,
			"podIPs": buildPodIPs(ipCR),
		},
	}
	body, err := jsonMarshal(patch)
	if err != nil {
		return err
	}
	if _, err := c.config.KubeClient.CoreV1().Pods(pod.Namespace).Patch(context.Background(), pod.Name,
		types.StrategicMergePatchType, body, metav1.PatchOptions{}, "status"); err != nil {
		return fmt.Errorf("patch pod %s/%s status: %w", pod.Namespace, pod.Name, err)
	}
	klog.Infof("patched pod %s/%s status.podIP=%s", pod.Namespace, pod.Name, primary)
	return nil
}

func buildPodIPs(ipCR *kubeovnv1.IP) []map[string]string {
	var out []map[string]string
	if ipCR.Spec.V4IPAddress != "" {
		out = append(out, map[string]string{"ip": ipCR.Spec.V4IPAddress})
	}
	if ipCR.Spec.V6IPAddress != "" {
		out = append(out, map[string]string{"ip": ipCR.Spec.V6IPAddress})
	}
	return out
}
