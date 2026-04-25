package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/ipam"
	"github.com/kubeovn/kube-ovn/pkg/util"
)

// handleSubnetReIPCandidates is invoked from handleAddOrUpdateSubnet whenever
// IPAM reports pods whose previously-assigned IPs fell outside the new CIDR.
//
// Behavior is gated on Subnet.spec.allowLiveReIP. When false (default), each
// candidate is annotated with NeedsReIPEvictionAnnotation and an event is
// emitted; the operator decides when to recreate the pod. This mirrors the
// upstream / Cilium-style "pod IP is immutable" posture.
//
// When allowLiveReIP=true the controller allocates a new IP from the new CIDR,
// rewrites the OVN logical-switch port, updates the IP CR, and patches the pod
// annotations with the new address/cidr/gateway. The kube-ovn-cni daemon on
// the owning node observes the IP CR change and reconfigures the pod's veth
// in-place (see pkg/daemon/ip_reassign_linux.go).
//
// In-flight TCP connections do not survive the swap; only process identity and
// future-connection reachability are preserved.
func (c *Controller) handleSubnetReIPCandidates(subnet *kubeovnv1.Subnet, candidates []ipam.ReIPCandidate) error {
	if !subnet.Spec.AllowLiveReIP {
		klog.Warningf("subnet %s cidr changed but allowLiveReIP=false; %d pod(s) hold stale IPs and need recreation", subnet.Name, len(candidates))
		var errs []error
		for _, cand := range candidates {
			if err := c.markPodForReIPEviction(cand); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}

	klog.Infof("subnet %s cidr changed with allowLiveReIP=true; re-IPing %d pod(s)", subnet.Name, len(candidates))
	var errs []error
	for _, cand := range candidates {
		if err := c.reIPPod(subnet, cand); err != nil {
			klog.Errorf("failed to re-IP pod %s on subnet %s: %v", cand.PodKey, subnet.Name, err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// markPodForReIPEviction sets an annotation on the pod so an external operator
// (or a human) can choose when to evict it. Idempotent.
func (c *Controller) markPodForReIPEviction(cand ipam.ReIPCandidate) error {
	ns, name, ok := splitPodKey(cand.PodKey)
	if !ok {
		klog.Warningf("re-IP candidate %q is not a pod key; skipping eviction marker", cand.PodKey)
		return nil
	}
	pod, err := c.podsLister.Pods(ns).Get(name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if pod.Annotations[util.NeedsReIPEvictionAnnotation] == "true" {
		return nil
	}
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:"true"}}}`, util.NeedsReIPEvictionAnnotation)
	if _, err := c.config.KubeClient.CoreV1().Pods(ns).Patch(context.Background(), name,
		types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("annotate pod %s/%s for re-IP eviction: %w", ns, name, err)
	}
	klog.Warningf("annotated pod %s/%s with %s=true (held stale IP after subnet CIDR change)", ns, name, util.NeedsReIPEvictionAnnotation)
	return nil
}

// reIPPod allocates a fresh address from the (already updated) subnet, rewrites
// the LSP, updates the IP CR, and patches pod annotations. The kube-ovn-cni
// daemon picks up the IP CR change and reconfigures the netns.
func (c *Controller) reIPPod(subnet *kubeovnv1.Subnet, cand ipam.ReIPCandidate) error {
	ns, name, ok := splitPodKey(cand.PodKey)
	if !ok {
		klog.Warningf("re-IP candidate %q is not a pod key; skipping", cand.PodKey)
		return nil
	}
	pod, err := c.podsLister.Pods(ns).Get(name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			klog.Infof("re-IP candidate pod %s/%s no longer exists; skipping", ns, name)
			return nil
		}
		return err
	}
	if pod.DeletionTimestamp != nil {
		klog.Infof("re-IP candidate pod %s/%s is being deleted; skipping", ns, name)
		return nil
	}

	// Statefulset / sticky pods pin a specific IP via annotation. If that IP is
	// out of the new CIDR, refuse to silently change it — the operator must
	// either pick a CIDR that contains it or drop the pin.
	if pod.Annotations[fmt.Sprintf(util.IPAddressAnnotationTemplate, subnet.Spec.Provider)] != "" &&
		isStaticIPPod(pod) {
		klog.Errorf("pod %s/%s has a sticky IP and cannot be live re-IP'd; mark for eviction", ns, name)
		return c.markPodForReIPEviction(cand)
	}

	mac := cand.Mac
	macPtr := &mac
	if mac == "" {
		macPtr = nil
	}

	v4, v6, newMac, err := c.ipam.GetRandomAddress(cand.PodKey, cand.NicName, macPtr, subnet.Name, "", nil, true)
	if err != nil {
		return fmt.Errorf("allocate replacement ip for pod %s on subnet %s: %w", cand.PodKey, subnet.Name, err)
	}
	klog.Infof("re-IP pod %s on subnet %s: %s/%s -> %s/%s (mac %s)", cand.PodKey, subnet.Name, cand.OldV4, cand.OldV6, v4, v6, newMac)

	// Rewrite the OVN logical switch port. CreateLogicalSwitchPort is
	// idempotent and updates Addresses/PortSecurity in place when the LSP
	// already exists.
	ipStr := util.GetStringIP(v4, v6)
	if err := c.OVNNbClient.CreateLogicalSwitchPort(subnet.Name, cand.NicName, ipStr, newMac, name, ns,
		true, "", "", false, nil, subnet.Spec.Vpc); err != nil {
		return fmt.Errorf("update lsp %s: %w", cand.NicName, err)
	}

	podType := getPodType(pod)
	if err := c.createOrUpdateIPCR(cand.NicName, name, ipStr, newMac, subnet.Name, ns, pod.Spec.NodeName, podType); err != nil {
		return fmt.Errorf("update ip CR %s: %w", cand.NicName, err)
	}

	// Patch pod annotations so daemon/observers see the new addressing. The
	// daemon's IP CR informer is the actual trigger for netns reconfiguration;
	// these annotations are kept in sync to match the rest of kube-ovn's
	// post-allocation pod state.
	if err := c.patchPodReIPAnnotations(pod, subnet, ipStr, newMac); err != nil {
		return fmt.Errorf("patch pod annotations: %w", err)
	}
	return nil
}

func (c *Controller) patchPodReIPAnnotations(pod *v1.Pod, subnet *kubeovnv1.Subnet, ipStr, mac string) error {
	provider := subnet.Spec.Provider
	if provider == "" {
		provider = util.OvnProvider
	}
	annotations := map[string]string{
		fmt.Sprintf(util.IPAddressAnnotationTemplate, provider):  ipStr,
		fmt.Sprintf(util.MacAddressAnnotationTemplate, provider): mac,
		fmt.Sprintf(util.CidrAnnotationTemplate, provider):       subnet.Spec.CIDRBlock,
		fmt.Sprintf(util.GatewayAnnotationTemplate, provider):    subnet.Spec.Gateway,
	}
	// Keep the legacy provider-less keys in sync for the default OVN provider.
	if provider == util.OvnProvider {
		annotations[util.IPAddressAnnotation] = ipStr
		annotations[util.MacAddressAnnotation] = mac
		annotations[util.CidrAnnotation] = subnet.Spec.CIDRBlock
		annotations[util.GatewayAnnotation] = subnet.Spec.Gateway
	}
	delete(pod.Annotations, util.NeedsReIPEvictionAnnotation)

	parts := make([]string, 0, len(annotations)+1)
	for k, v := range annotations {
		parts = append(parts, fmt.Sprintf("%q:%q", k, v))
	}
	parts = append(parts, fmt.Sprintf("%q:null", util.NeedsReIPEvictionAnnotation))
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%s}}}`, strings.Join(parts, ","))
	if _, err := c.config.KubeClient.CoreV1().Pods(pod.Namespace).Patch(context.Background(), pod.Name,
		types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		return err
	}
	return nil
}

func splitPodKey(key string) (namespace, name string, ok bool) {
	parts := strings.SplitN(key, "/", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func isStaticIPPod(pod *v1.Pod) bool {
	for k := range pod.Annotations {
		if strings.HasSuffix(k, ".kubernetes.io/ip_address") || k == util.IPAddressAnnotation {
			// allocation paths set IPAddressAnnotation post-bind; sticky pods
			// have it pre-set via the user-supplied annotation. We can only
			// tell by inspecting whether the pod is sts/vm with KeepIP.
			if isSts, _, _ := isStatefulSetPod(pod); isSts {
				return true
			}
			if isVM, _ := isVMPod(pod); isVM {
				return true
			}
		}
	}
	return false
}
