# DRA NICs (experimental)

Secondary pod NICs can be requested through [Dynamic Resource Allocation](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/) (DRA) instead of a Multus network attachment. A DRA driver publishes Kube-OVN subnets as devices, the scheduler allocates them to a pod's `ResourceClaim`, and kube-ovn-controller treats every allocated device like a Multus attachment: it allocates the address and creates the logical switch port, ip CR and pod annotations through the regular pod allocation path.

The feature is experimental and disabled by default. The contract below may still change.

## Enabling

- kube-ovn-controller flag `--enable-dra-nic` (`ENABLE_DRA_NIC` in `install.sh` and the legacy chart, `features.enableDraNic` in the v2 chart)
- `--dra-nic-driver-name` selects the DRA driver whose devices are NICs, default `nic.kubeovn.io`
- Requires the `resource.k8s.io/v1` API (Kubernetes 1.34 or later). The controller only watches ResourceClaims and ResourceSlices when the feature is enabled.

## Driver contract

A device is a Kube-OVN NIC when it is allocated by the configured driver. kube-ovn-controller reads:

- the device attribute `subnetName` (`<driver>/subnetName` or unqualified) from the newest ResourceSlice generation of the device's pool; it names the subnet to allocate from
- the subnet's `spec.provider`, which must be a dedicated provider (for example `<name>.<namespace>.ovn`, the convention of a NetworkAttachmentDefinition). The default provider `ovn` belongs to the pod's primary NIC and is rejected.
- for several NICs on one provider in a pod: `interfaceName` from the driver's opaque claim config that applies to the request

Each NIC is keyed by a provider key, which is the subnet's provider, or `<provider>.<interfaceName>` when several NICs of the pod share the provider (the same rule Kube-OVN uses for repeated attachments of one NAD). A DRA NIC must not use the provider of a Multus attachment of the same pod.

Once the pod is bound to a node and all its claims are allocated, kube-ovn-controller

- creates the logical switch port and ip CR `<name>.<namespace>.<provider key>`, where `<name>` is the pod name, or the VM name for KubeVirt VM pods with keep-vm-ip
- writes the pod annotations `<provider key>.kubernetes.io/{ip_address,mac_address,cidr,gateway,logical_switch,allocated}`, and `virtualmachine` when the port is named after a VM. Dual-stack values are comma separated.

The driver is expected to

1. publish the subnets as devices with the `subnetName` attribute
2. in `NodePrepareResources`, wait for `<provider key>.kubernetes.io/allocated: "true"` and read the address annotations
3. plug the interface into the pod network namespace and attach its host end to `br-int` with `external_ids:iface-id=<logical switch port name>`
4. not release anything on unprepare: kube-ovn-controller releases the address when the pod is deleted, or keeps it for a keep-vm-ip VM, as for Multus attachments

## Lifecycle

- Pod deletion releases the NIC's port, ip CR and address, also when the claim's reservation is already gone.
- KubeVirt VMs with keep-vm-ip keep the IP and MAC of their DRA NICs across restarts and while stopped, like their Multus attachments.

## Implementations

- [kube-ovn-dra-driver](https://github.com/soer3n/kube-ovn-dra-driver) is a reference driver that plugs the NICs through an NRI hook, including the tap and DHCP setup KubeVirt VMs need.
- [kube-ovn-network-binding-plugin](https://github.com/soer3n/kube-ovn-network-binding-plugin) attaches such NICs to KubeVirt VM domains, since KubeVirt's built-in bindings do not resolve DRA networks yet.

## Testing

`make kube-ovn-dra-e2e` runs against a cluster installed with `ENABLE_DRA_NIC=true` and skips otherwise. It covers only the kube-ovn-controller side: it publishes a static ResourceSlice, so no driver is needed, and the pod never starts, so no nic is plugged.
