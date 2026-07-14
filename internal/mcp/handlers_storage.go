package mcp

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/snapp-incubator/openshift-mcp/internal/k8s"
)

const defaultClassAnnotation = "storageclass.kubernetes.io/is-default-class"

type storageClasses struct {
	entries      []map[string]any
	defaultClass string
	warning      string
}

func storageClassSummaries(ctx context.Context, c *k8s.Client) (*storageClasses, error) {
	list, err := c.Clientset.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list storageclasses: %w", err)
	}
	res := &storageClasses{entries: make([]map[string]any, 0, len(list.Items))}
	var defaults []string
	for i := range list.Items {
		sc := &list.Items[i]
		isDefault := sc.Annotations[defaultClassAnnotation] == "true"
		if isDefault {
			defaults = append(defaults, sc.Name)
		}
		entry := map[string]any{
			"name": sc.Name, "provisioner": sc.Provisioner, "default": isDefault,
		}
		if sc.ReclaimPolicy != nil {
			entry["reclaim_policy"] = string(*sc.ReclaimPolicy)
		}
		if sc.VolumeBindingMode != nil {
			entry["volume_binding_mode"] = string(*sc.VolumeBindingMode)
			if *sc.VolumeBindingMode == "WaitForFirstConsumer" {
				entry["note"] = "PVCs on this class stay Pending until a pod using them is scheduled — expected, not a fault"
			}
		}
		res.entries = append(res.entries, entry)
	}

	switch len(defaults) {
	case 0:
		res.warning = "No default StorageClass. A PVC that omits storageClassName will stay Pending forever."
	case 1:
		res.defaultClass = defaults[0]
	default:
		res.warning = fmt.Sprintf(
			"%d StorageClasses are marked default (%v). Kubernetes rejects PVCs that omit storageClassName when this is ambiguous.",
			len(defaults), defaults)
	}
	return res, nil
}
