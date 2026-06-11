package driver

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestNextFreeVNI(t *testing.T) {
	tests := []struct {
		name     string
		assigned map[int]bool
		start    int
		end      int
		wantVNI  int
		wantOK   bool
	}{
		{"empty range picks start", map[int]bool{}, 10, 12, 10, true},
		{"skips assigned", map[int]bool{10: true, 11: true}, 10, 12, 12, true},
		{"gap in middle", map[int]bool{10: true, 12: true}, 10, 12, 11, true},
		{"exhausted", map[int]bool{10: true, 11: true, 12: true}, 10, 12, 0, false},
		{"single free", map[int]bool{}, 5, 5, 5, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vni, ok := nextFreeVNI(tc.assigned, tc.start, tc.end)
			if ok != tc.wantOK || (ok && vni != tc.wantVNI) {
				t.Fatalf("nextFreeVNI(%v,%d,%d) = (%d,%v), want (%d,%v)",
					tc.assigned, tc.start, tc.end, vni, ok, tc.wantVNI, tc.wantOK)
			}
		})
	}
}

func TestReconcileNamespaceAssignsVNI(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}},
		// pre-existing assignment must be respected, not reassigned
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:        "team-b",
			Annotations: map[string]string{vniAnnotationKey: "10"},
		}},
		// system namespace must be skipped
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
	)
	c := &namespaceVNIController{
		client: client,
		start:  10,
		end:    20,
		skip:   defaultVNISkip(),
	}

	// team-a is unassigned; lowest free VNI given team-b=10 is 11.
	if err := c.reconcile(context.Background(), "team-a"); err != nil {
		t.Fatalf("reconcile team-a: %v", err)
	}
	ns, _ := client.CoreV1().Namespaces().Get(context.Background(), "team-a", metav1.GetOptions{})
	if got := ns.Annotations[vniAnnotationKey]; got != "11" {
		t.Fatalf("team-a VNI = %q, want %q", got, "11")
	}

	// kube-system must remain unassigned.
	if err := c.reconcile(context.Background(), "kube-system"); err != nil {
		t.Fatalf("reconcile kube-system: %v", err)
	}
	sys, _ := client.CoreV1().Namespaces().Get(context.Background(), "kube-system", metav1.GetOptions{})
	if _, ok := sys.Annotations[vniAnnotationKey]; ok {
		t.Fatalf("kube-system unexpectedly got a VNI")
	}

	// re-reconciling team-b must not change its VNI.
	if err := c.reconcile(context.Background(), "team-b"); err != nil {
		t.Fatalf("reconcile team-b: %v", err)
	}
	b, _ := client.CoreV1().Namespaces().Get(context.Background(), "team-b", metav1.GetOptions{})
	if got := b.Annotations[vniAnnotationKey]; got != "10" {
		t.Fatalf("team-b VNI changed to %q, want 10", got)
	}
}
