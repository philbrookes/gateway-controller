package controller

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	proxyv1alpha1 "github.com/praxis-proxy/gateway-controller/api/proxy/v1alpha1"
)

func TestServiceIPFamilies(t *testing.T) {
	t.Parallel()

	bind := func(addr string) *string { return &addr }
	tests := []struct {
		name       string
		binds      []*string
		wantFamily []corev1.IPFamily
		wantPolicy corev1.IPFamilyPolicy
	}{
		{
			name:       "default bind selects IPv4",
			binds:      []*string{nil},
			wantFamily: []corev1.IPFamily{corev1.IPv4Protocol},
			wantPolicy: corev1.IPFamilyPolicySingleStack,
		},
		{
			name:       "IPv4 wildcard selects IPv4",
			binds:      []*string{bind("0.0.0.0")},
			wantFamily: []corev1.IPFamily{corev1.IPv4Protocol},
			wantPolicy: corev1.IPFamilyPolicySingleStack,
		},
		{
			name:       "IPv4 literal selects IPv4",
			binds:      []*string{bind("127.0.0.1")},
			wantFamily: []corev1.IPFamily{corev1.IPv4Protocol},
			wantPolicy: corev1.IPFamilyPolicySingleStack,
		},
		{
			name:       "IPv6 literal selects IPv6",
			binds:      []*string{bind("fd00::1")},
			wantFamily: []corev1.IPFamily{corev1.IPv6Protocol},
			wantPolicy: corev1.IPFamilyPolicySingleStack,
		},
		{
			name:       "dual-stack wildcard accepts both families",
			binds:      []*string{bind("::")},
			wantFamily: []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol},
			wantPolicy: corev1.IPFamilyPolicyPreferDualStack,
		},
		{
			name:       "mixed binds request both families",
			binds:      []*string{bind("0.0.0.0"), bind("fd00::1")},
			wantFamily: []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol},
			wantPolicy: corev1.IPFamilyPolicyPreferDualStack,
		},
		{
			name:       "unparseable bind treated as the IPv4 default",
			binds:      []*string{bind("listener.example.com")},
			wantFamily: []corev1.IPFamily{corev1.IPv4Protocol},
			wantPolicy: corev1.IPFamilyPolicySingleStack,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			proxy := guardTestProxy()
			proxy.Spec.Listeners = nil
			for i, b := range tt.binds {
				proxy.Spec.Listeners = append(proxy.Spec.Listeners, proxyv1alpha1.PraxisListener{
					Name: fmt.Sprintf("l%d", i), Port: 8080, BindAddress: b, FilterChains: []string{"chain"},
				})
			}
			families, policy := serviceIPFamilies(proxy)
			if len(families) != len(tt.wantFamily) {
				t.Fatalf("families = %v, want %v", families, tt.wantFamily)
			}
			for i := range tt.wantFamily {
				if families[i] != tt.wantFamily[i] {
					t.Errorf("families = %v, want %v", families, tt.wantFamily)
				}
			}
			if policy != tt.wantPolicy {
				t.Errorf("policy = %s, want %s", policy, tt.wantPolicy)
			}
		})
	}
}

func TestReconcileService_IPFamilies(t *testing.T) {
	t.Parallel()

	v6Bind := "fd00::1"
	tests := []struct {
		name       string
		bind       *string
		wantFamily corev1.IPFamily
	}{
		{name: "default bind publishes an IPv4 Service", bind: nil, wantFamily: corev1.IPv4Protocol},
		{name: "IPv6 bind publishes an IPv6 Service", bind: &v6Bind, wantFamily: corev1.IPv6Protocol},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			proxy := guardTestProxy()
			proxy.Spec.Listeners = []proxyv1alpha1.PraxisListener{{
				Name: "http", Port: 8080, BindAddress: tt.bind, FilterChains: []string{"chain"},
			}}
			scheme := proxyTestScheme(t)
			cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(proxy).Build()
			r := &ProxyReconciler{Client: cl, Scheme: scheme}

			if err := r.reconcileService(context.Background(), proxy); err != nil {
				t.Fatalf("reconcile service: %v", err)
			}
			svc := &corev1.Service{}
			if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "gw"}, svc); err != nil {
				t.Fatalf("fetch service: %v", err)
			}
			if len(svc.Spec.IPFamilies) != 1 || svc.Spec.IPFamilies[0] != tt.wantFamily {
				t.Errorf("ipFamilies = %v, want [%s]", svc.Spec.IPFamilies, tt.wantFamily)
			}
			if svc.Spec.IPFamilyPolicy == nil || *svc.Spec.IPFamilyPolicy != corev1.IPFamilyPolicySingleStack {
				t.Errorf("ipFamilyPolicy = %v, want SingleStack", svc.Spec.IPFamilyPolicy)
			}
		})
	}
}
