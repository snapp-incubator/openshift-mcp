package k8s

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// PageSize is the number of items fetched per request in cluster-wide lists.
const PageSize = 500

// ListPages calls list once per page until the server has returned
// everything, so large lists are never cut short.
func ListPages(list func(opts metav1.ListOptions) (next string, err error)) error {
	opts := metav1.ListOptions{Limit: PageSize}
	for {
		next, err := list(opts)
		if err != nil {
			return err
		}
		if next == "" {
			return nil
		}
		opts.Continue = next
	}
}

// ListCustom lists a custom resource in all namespaces, using the first of
// versions the cluster serves. If the CRD is not installed, it returns
// installed=false and no error.
func (c *Client) ListCustom(ctx context.Context, group, resource string, versions []string,
	each func(*unstructured.Unstructured)) (installed bool, err error) {
	for _, v := range versions {
		gvr := schema.GroupVersionResource{Group: group, Version: v, Resource: resource}
		err := ListPages(func(opts metav1.ListOptions) (string, error) {
			list, err := c.Dynamic.Resource(gvr).List(ctx, opts)
			if err != nil {
				return "", err
			}
			for i := range list.Items {
				each(&list.Items[i])
			}
			return list.GetContinue(), nil
		})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("list %s: %w", gvr, err)
		}
		return true, nil
	}
	return false, nil
}
