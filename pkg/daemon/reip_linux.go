package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/ovs"
)

// jsonMarshal is a thin wrapper to keep reip.go OS-agnostic.
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// reassignPodNetns enters the target pod's netns and replaces the interface's
// addresses and default route to match the new IP CR. Idempotent: if the netns
// already matches, this is a no-op.
//
// The OVS port itself is *not* recreated — only the in-netns address. The
// controller has already updated the OVN logical switch port via
// CreateLogicalSwitchPort (which is idempotent on update).
func (c *Controller) reassignPodNetns(pod *v1.Pod, subnet *kubeovnv1.Subnet, newIP, newMac string) error {
	hostNic, ifName, podNetns, err := c.lookupPodOvsInterface(pod)
	if err != nil {
		return err
	}
	if podNetns == "" {
		klog.Warningf("no pod_netns external_id on host nic for pod %s/%s; skipping (CNI ADD likely not yet complete)", pod.Namespace, pod.Name)
		return nil
	}

	mac, err := net.ParseMAC(newMac)
	if err != nil {
		return fmt.Errorf("invalid mac %q for pod %s/%s: %w", newMac, pod.Namespace, pod.Name, err)
	}

	// Refresh OVS interface external_ids:ip so subsequent CNI/inspection sees
	// the new mapping. Best-effort.
	if hostNic != "" {
		if _, err := ovs.Exec("set", "interface", hostNic, "external_ids:ip="+newIP); err != nil {
			klog.Warningf("failed to update ovs interface %s external_ids:ip=%s: %v", hostNic, newIP, err)
		}
	}

	gateway := subnet.Spec.Gateway
	mtu := int(subnet.Spec.Mtu)

	podNS, err := ns.GetNS(podNetns)
	if err != nil {
		return fmt.Errorf("open netns %s: %w", podNetns, err)
	}
	defer podNS.Close()

	return ns.WithNetNSPath(podNS.Path(), func(_ ns.NetNS) error {
		link, err := netlink.LinkByName(ifName)
		if err != nil {
			return fmt.Errorf("find link %s in netns %s: %w", ifName, podNetns, err)
		}

		// configureNic implements the idempotent address-diff logic we need
		// (list current, compute add/del set, apply). Reuse it as-is.
		if err := configureNic(ifName, newIP, mac, mtu, false, true, false, false); err != nil {
			return fmt.Errorf("reconfigure nic %s: %w", ifName, err)
		}

		// Replace the default route(s). RouteReplace is idempotent.
		for gw := range strings.SplitSeq(gateway, ",") {
			gw = strings.TrimSpace(gw)
			if gw == "" {
				continue
			}
			gwIP := net.ParseIP(gw)
			if gwIP == nil {
				klog.Warningf("invalid gateway %q on subnet %s", gw, subnet.Name)
				continue
			}
			if err := netlink.RouteReplace(&netlink.Route{
				LinkIndex: link.Attrs().Index,
				Scope:     netlink.SCOPE_UNIVERSE,
				Gw:        gwIP,
			}); err != nil {
				return fmt.Errorf("replace default route via %s: %w", gw, err)
			}
		}
		klog.Infof("re-IPed pod %s/%s in netns %s: ifName=%s newIP=%s gw=%s", pod.Namespace, pod.Name, podNetns, ifName, newIP, gateway)
		return nil
	})
}

// lookupPodOvsInterface finds the host-side OVS interface for the pod and
// returns (hostNicName, podNicName, podNetnsPath). Pulled from OVS external_ids
// keyed by iface-id (which is PodNameToPortName). Returns empty fields when no
// matching OVS interface exists yet.
func (c *Controller) lookupPodOvsInterface(pod *v1.Pod) (hostNic, ifName, podNetns string, err error) {
	// iface-id format is "<podName>.<ns>" or "<podName>.<ns>.<provider>" — we
	// search by prefix to avoid threading the provider here.
	prefix := fmt.Sprintf("iface-id=%s.%s", pod.Name, pod.Namespace)
	output, err := ovs.Exec("--data=bare", "--format=csv", "--no-heading",
		"--columns=name,external_ids", "find", "interface", `external_ids:iface-id!=[]`)
	if err != nil {
		return "", "", "", fmt.Errorf("ovs-vsctl find interface: %w", err)
	}
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, prefix) {
			continue
		}
		parts := strings.SplitN(line, ",", 2)
		if len(parts) != 2 {
			continue
		}
		hostNic = strings.TrimSpace(parts[0])
		for f := range strings.FieldsSeq(parts[1]) {
			f = strings.TrimSpace(f)
			if after, ok := strings.CutPrefix(f, "pod_netns="); ok {
				podNetns = after
			}
		}
		// ifName is whatever the pod calls it; default veth name is "eth0"
		// when configureContainerNic renames it. The actual in-pod interface
		// name is recoverable via OVS external_ids only when the operator
		// passes it through; in practice kube-ovn always uses "eth0" for the
		// primary nic. Multi-nic pods use the iface-id naming convention
		// embedded above. For now we restrict live re-IP to the primary nic.
		ifName = "eth0"
		return hostNic, ifName, podNetns, nil
	}
	return "", "", "", nil
}
