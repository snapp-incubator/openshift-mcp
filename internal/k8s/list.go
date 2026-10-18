package k8s

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// PageSize is the page size for cluster-wide lists. Large enough to keep the
// number of round trips low, small enough that one page of pods stays a few MB.
const PageSize = 500

// ListPages calls list once per page, following continue tokens until the API
// server has returned everything. Cluster-wide audits use it so their result
// is never silently truncated by a page limit.
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

// ListCustom lists a custom resource cluster-wide across every page, trying
// the versions in order and using the first one the API server serves.
// installed is false, and err nil, when it serves none of them: the CRD is
// absent on this cluster, which is a fact to report, not a failure.
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
