package k8s

import (
	"errors"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestListPagesFollowsContinue(t *testing.T) {
	pages := map[string]string{"": "a", "a": "b", "b": ""}
	var seen []string
	err := ListPages(func(opts metav1.ListOptions) (string, error) {
		if opts.Limit != PageSize {
			t.Errorf("limit = %d, want %d", opts.Limit, PageSize)
		}
		seen = append(seen, opts.Continue)
		return pages[opts.Continue], nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seen, []string{"", "a", "b"}) {
		t.Errorf("pages requested = %q, want every page once", seen)
	}
}

func TestListPagesStopsOnError(t *testing.T) {
	boom := errors.New("boom")
	calls := 0
	err := ListPages(func(metav1.ListOptions) (string, error) {
		calls++
		return "more", boom
	})
	if !errors.Is(err, boom) || calls != 1 {
		t.Errorf("err=%v calls=%d, want boom after one call", err, calls)
	}
}
