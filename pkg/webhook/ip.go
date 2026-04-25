package webhook

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	ovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/util"
)

var ipGVK = ovnv1.SchemeGroupVersion.WithKind(util.KindIP)

func (v *ValidatingHook) IPCreateHook(ctx context.Context, req admission.Request) admission.Response {
	ip := ovnv1.IP{}
	if err := v.decoder.DecodeRaw(req.Object, &ip); err != nil {
		return ctrlwebhook.Errored(http.StatusBadRequest, err)
	}

	if err := v.ValidateIP(ctx, &ip); err != nil {
		return ctrlwebhook.Errored(http.StatusBadRequest, err)
	}
	return ctrlwebhook.Allowed("bypass")
}

func (v *ValidatingHook) IPUpdateHook(ctx context.Context, req admission.Request) admission.Response {
	ipOld := ovnv1.IP{}
	if err := v.decoder.DecodeRaw(req.OldObject, &ipOld); err != nil {
		return ctrlwebhook.Errored(http.StatusBadRequest, err)
	}

	ipNew := ovnv1.IP{}
	if err := v.decoder.DecodeRaw(req.Object, &ipNew); err != nil {
		return ctrlwebhook.Errored(http.StatusBadRequest, err)
	}

	if err := v.ValidateIP(ctx, &ipNew); err != nil {
		return ctrlwebhook.Errored(http.StatusBadRequest, err)
	}

	// ip can not change these specs below
	if ipOld.Spec.Subnet != "" && ipNew.Spec.Subnet != ipOld.Spec.Subnet {
		err := fmt.Errorf("ip %s subnet can not change", ipNew.Name)
		return ctrlwebhook.Errored(http.StatusBadRequest, err)
	}
	if ipOld.Spec.Namespace != "" && ipNew.Spec.Namespace != ipOld.Spec.Namespace {
		err := fmt.Errorf("ip %s namespace can not change", ipNew.Name)
		return ctrlwebhook.Errored(http.StatusBadRequest, err)
	}
	if ipOld.Spec.PodName != "" && ipNew.Spec.PodName != ipOld.Spec.PodName {
		err := fmt.Errorf("ip %s podName can not change", ipNew.Name)
		return ctrlwebhook.Errored(http.StatusBadRequest, err)
	}
	if ipOld.Spec.PodType != "" && ipNew.Spec.PodType != ipOld.Spec.PodType {
		err := fmt.Errorf("ip %s podType can not change", ipNew.Name)
		return ctrlwebhook.Errored(http.StatusBadRequest, err)
	}
	// v4/v6 address mutation is normally rejected. The owning subnet may opt
	// in to live re-IP via Subnet.spec.allowLiveReIP, in which case an address
	// change is permitted as long as ValidateIP confirms the new address is in
	// the subnet's current CIDR. The MAC may also change when the subnet
	// reissues a fresh MAC alongside the new IP.
	v4Changed := ipOld.Spec.V4IPAddress != "" && ipNew.Spec.V4IPAddress != ipOld.Spec.V4IPAddress
	v6Changed := ipOld.Spec.V6IPAddress != "" && ipNew.Spec.V6IPAddress != ipOld.Spec.V6IPAddress
	macChanged := ipOld.Spec.MacAddress != "" && ipNew.Spec.MacAddress != ipOld.Spec.MacAddress
	if v4Changed || v6Changed || macChanged {
		subnet := &ovnv1.Subnet{}
		if err := v.cache.Get(ctx, client.ObjectKey{Name: ipNew.Spec.Subnet}, subnet); err != nil {
			return ctrlwebhook.Errored(http.StatusBadRequest, err)
		}
		if !subnet.Spec.AllowLiveReIP {
			switch {
			case v4Changed:
				return ctrlwebhook.Errored(http.StatusBadRequest, fmt.Errorf("ip %s v4IPAddress can not change (set subnet.spec.allowLiveReIP=true to permit)", ipNew.Name))
			case v6Changed:
				return ctrlwebhook.Errored(http.StatusBadRequest, fmt.Errorf("ip %s v6IPAddress can not change (set subnet.spec.allowLiveReIP=true to permit)", ipNew.Name))
			case macChanged:
				return ctrlwebhook.Errored(http.StatusBadRequest, fmt.Errorf("ip %s macAddress can not change (set subnet.spec.allowLiveReIP=true to permit)", ipNew.Name))
			}
		}
	}
	return ctrlwebhook.Allowed("bypass")
}

func (v *ValidatingHook) ValidateIP(ctx context.Context, ip *ovnv1.IP) error {
	if ip.Spec.Subnet == "" {
		return errors.New("subnet parameter cannot be empty")
	}

	subnet := &ovnv1.Subnet{}
	key := client.ObjectKey{Name: ip.Spec.Subnet}
	if err := v.cache.Get(ctx, key, subnet); err != nil {
		return err
	}

	if ip.Spec.V4IPAddress != "" {
		if net.ParseIP(ip.Spec.V4IPAddress) == nil {
			err := fmt.Errorf("%s is not a valid ip", ip.Spec.V4IPAddress)
			return err
		}

		if !util.CIDRContainIP(subnet.Spec.CIDRBlock, ip.Spec.V4IPAddress) {
			err := fmt.Errorf("the V4ip %s is not in the range of subnet %s, cidr %v",
				ip.Spec.V4IPAddress, subnet.Name, subnet.Spec.CIDRBlock)
			return err
		}
	}

	if ip.Spec.V6IPAddress != "" {
		// v6 ip address can not use upper case
		if util.ContainsUppercase(ip.Spec.V6IPAddress) {
			err := fmt.Errorf("ip %s v6 ip address can not contain upper case", ip.Name)
			return err
		}

		if net.ParseIP(ip.Spec.V6IPAddress) == nil {
			err := fmt.Errorf("%s is not a valid ip", ip.Spec.V6IPAddress)
			return err
		}

		if !util.CIDRContainIP(subnet.Spec.CIDRBlock, ip.Spec.V6IPAddress) {
			err := fmt.Errorf("the ip %s is not in the range of subnet %s, cidr %v",
				ip.Spec.V6IPAddress, subnet.Name, subnet.Spec.CIDRBlock)
			return err
		}
	}

	if ip.Spec.Subnet == "" {
		return errors.New("subnet parameter cannot be empty")
	}

	if ip.Spec.PodType != "" &&
		ip.Spec.PodType != util.KindVirtualMachine &&
		ip.Spec.PodType != util.KindStatefulSet {
		return fmt.Errorf("podType %s is not supported", ip.Spec.PodType)
	}

	return nil
}
