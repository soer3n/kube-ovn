package controller

import (
	"context"
	"reflect"
	"slices"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/util"
)

func (c *Controller) enqueueAddIPPool(obj any) {
	key := cache.MetaObjectToName(obj.(*kubeovnv1.IPPool)).String()
	klog.V(3).Infof("enqueue add ippool %s", key)
	c.addOrUpdateIPPoolQueue.Add(key)
}

func (c *Controller) enqueueDeleteIPPool(obj any) {
	ippool := obj.(*kubeovnv1.IPPool)
	klog.V(3).Infof("enqueue delete ippool %s", cache.MetaObjectToName(ippool).String())
	c.deleteIPPoolQueue.Add(ippool)
}

func (c *Controller) enqueueUpdateIPPool(oldObj, newObj any) {
	oldIPPool := oldObj.(*kubeovnv1.IPPool)
	newIPPool := newObj.(*kubeovnv1.IPPool)
	if !slices.Equal(oldIPPool.Spec.Namespaces, newIPPool.Spec.Namespaces) ||
		!slices.Equal(oldIPPool.Spec.IPs, newIPPool.Spec.IPs) {
		key := cache.MetaObjectToName(newIPPool).String()
		klog.V(3).Infof("enqueue update ippool %s", key)
		c.addOrUpdateIPPoolQueue.Add(key)
	}
}

func (c *Controller) handleAddOrUpdateIPPool(key string) error {
	c.ippoolKeyMutex.LockKey(key)
	defer func() { _ = c.ippoolKeyMutex.UnlockKey(key) }()

	cachedIPPool, err := c.ippoolLister.Get(key)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		klog.Error(err)
		return err
	}
	klog.Infof("handle add/update ippool %s", cachedIPPool.Name)

	ippool := cachedIPPool.DeepCopy()
	ippool.Status.EnsureStandardConditions()
	if err = c.ipam.AddOrUpdateIPPool(ippool.Spec.Subnet, ippool.Name, ippool.Spec.IPs); err != nil {
		klog.Errorf("failed to add/update ippool %s with IPs %v in subnet %s: %v", ippool.Name, ippool.Spec.IPs, ippool.Spec.Subnet, err)
		if patchErr := c.patchIPPoolStatusCondition(ippool, "UpdateIPAMFailed", err.Error()); patchErr != nil {
			klog.Error(patchErr)
		}
		return err
	}

	v4a, v4u, v6a, v6u, v4as, v4us, v6as, v6us := c.ipam.IPPoolStatistics(ippool.Spec.Subnet, ippool.Name)
	ippool.Status.V4AvailableIPs = v4a
	ippool.Status.V4UsingIPs = v4u
	ippool.Status.V6AvailableIPs = v6a
	ippool.Status.V6UsingIPs = v6u
	ippool.Status.V4AvailableIPRange = v4as
	ippool.Status.V4UsingIPRange = v4us
	ippool.Status.V6AvailableIPRange = v6as
	ippool.Status.V6UsingIPRange = v6us

	if err = c.patchIPPoolStatusCondition(ippool, "UpdateIPAMSucceeded", ""); err != nil {
		klog.Error(err)
		return err
	}

	for _, ns := range ippool.Spec.Namespaces {
		c.addNamespaceQueue.Add(ns)
	}

	return nil
}

func (c *Controller) handleDeleteIPPool(ippool *kubeovnv1.IPPool) error {
	c.ippoolKeyMutex.LockKey(ippool.Name)
	defer func() { _ = c.ippoolKeyMutex.UnlockKey(ippool.Name) }()

	klog.Infof("handle delete ippool %s", ippool.Name)
	c.ipam.RemoveIPPool(ippool.Spec.Subnet, ippool.Name)

	namespaces, err := c.namespacesLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list namespaces: %v", err)
		return err
	}

	for _, ns := range namespaces {
		if len(ns.Annotations) == 0 {
			continue
		}
		if ns.Annotations[util.IPPoolAnnotation] == ippool.Name {
			c.enqueueAddNamespace(ns)
		}
	}

	return nil
}

func (c *Controller) handleUpdateIPPoolStatus(key string) error {
	c.ippoolKeyMutex.LockKey(key)
	defer func() { _ = c.ippoolKeyMutex.UnlockKey(key) }()

	cachedIPPool, err := c.ippoolLister.Get(key)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		klog.Error(err)
		return err
	}

	ippool := cachedIPPool.DeepCopy()
	v4a, v4u, v6a, v6u, v4as, v4us, v6as, v6us := c.ipam.IPPoolStatistics(ippool.Spec.Subnet, ippool.Name)
	ippool.Status.V4AvailableIPs = v4a
	ippool.Status.V4UsingIPs = v4u
	ippool.Status.V6AvailableIPs = v6a
	ippool.Status.V6UsingIPs = v6u
	ippool.Status.V4AvailableIPRange = v4as
	ippool.Status.V4UsingIPRange = v4us
	ippool.Status.V6AvailableIPRange = v6as
	ippool.Status.V6UsingIPRange = v6us
	if reflect.DeepEqual(ippool.Status, cachedIPPool.Status) {
		return nil
	}

	return c.patchIPPoolStatus(ippool)
}

func (c Controller) patchIPPoolStatusCondition(ippool *kubeovnv1.IPPool, reason, errMsg string) error {
	if errMsg != "" {
		ippool.Status.SetError(reason, errMsg)
		ippool.Status.NotReady(reason, errMsg)
		c.recorder.Eventf(ippool, corev1.EventTypeWarning, reason, errMsg)
	} else {
		ippool.Status.Ready(reason, "")
		c.recorder.Eventf(ippool, corev1.EventTypeNormal, reason, errMsg)
	}

	return c.patchIPPoolStatus(ippool)
}

func (c Controller) patchIPPoolStatus(ippool *kubeovnv1.IPPool) error {
	bytes, err := ippool.Status.Bytes()
	if err != nil {
		klog.Errorf("failed to generate json representation for status of ippool %s: %v", ippool.Name, err)
		return err
	}
	if _, err = c.config.KubeOvnClient.KubeovnV1().IPPools().Patch(context.Background(), ippool.Name, types.MergePatchType, bytes, metav1.PatchOptions{}, "status"); err != nil {
		klog.Errorf("failed to patch status of ippool %s: %v", ippool.Name, err)
		return err
	}

	return nil
}
