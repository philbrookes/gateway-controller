package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
			name:       "dual-stack wildcard lets the cluster choose families",
			binds:      []*string{bind("::")},
			wantFamily: nil,
			wantPolicy: corev1.IPFamilyPolicyPreferDualStack,
		},
		{
			name:       "dual-stack wildcard next to an IPv4 bind stays IPv4",
			binds:      []*string{bind("::"), bind("0.0.0.0")},
			wantFamily: []corev1.IPFamily{corev1.IPv4Protocol},
			wantPolicy: corev1.IPFamilyPolicySingleStack,
		},
		{
			name:       "dual-stack wildcard next to an IPv6 bind stays IPv6",
			binds:      []*string{bind("::"), bind("fd00::1")},
			wantFamily: []corev1.IPFamily{corev1.IPv6Protocol},
			wantPolicy: corev1.IPFamilyPolicySingleStack,
		},
		{
			name:       "mixed single-family binds request both families",
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
	wildcardBind := "::"
	tests := []struct {
		name       string
		bind       *string
		wantFamily []corev1.IPFamily
		wantPolicy corev1.IPFamilyPolicy
	}{
		{
			name:       "default bind publishes an IPv4 Service",
			bind:       nil,
			wantFamily: []corev1.IPFamily{corev1.IPv4Protocol},
			wantPolicy: corev1.IPFamilyPolicySingleStack,
		},
		{
			name:       "IPv6 bind publishes an IPv6 Service",
			bind:       &v6Bind,
			wantFamily: []corev1.IPFamily{corev1.IPv6Protocol},
			wantPolicy: corev1.IPFamilyPolicySingleStack,
		},
		{
			name:       "dual-stack wildcard leaves the families for the cluster to choose",
			bind:       &wildcardBind,
			wantFamily: nil,
			wantPolicy: corev1.IPFamilyPolicyPreferDualStack,
		},
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
			if len(svc.Spec.IPFamilies) != len(tt.wantFamily) {
				t.Errorf("ipFamilies = %v, want %v", svc.Spec.IPFamilies, tt.wantFamily)
			}
			for i := range tt.wantFamily {
				if svc.Spec.IPFamilies[i] != tt.wantFamily[i] {
					t.Errorf("ipFamilies = %v, want %v", svc.Spec.IPFamilies, tt.wantFamily)
				}
			}
			if svc.Spec.IPFamilyPolicy == nil || *svc.Spec.IPFamilyPolicy != tt.wantPolicy {
				t.Errorf("ipFamilyPolicy = %v, want %s", svc.Spec.IPFamilyPolicy, tt.wantPolicy)
			}
		})
	}
}

func TestRetainedFamilyConflict(t *testing.T) {
	t.Parallel()

	bind := func(addr string) *string { return &addr }
	tests := []struct {
		name        string
		families    []corev1.IPFamily
		binds       []*string
		wantMessage []string
	}{
		{
			name:     "no retained families constrains nothing",
			families: nil,
			binds:    []*string{bind("fd00::1")},
		},
		{
			name:     "an IPv6 Service accepts the dual-stack wildcard",
			families: []corev1.IPFamily{corev1.IPv6Protocol},
			binds:    []*string{bind("::")},
		},
		{
			name:     "an IPv6 Service accepts an IPv6 bind",
			families: []corev1.IPFamily{corev1.IPv6Protocol},
			binds:    []*string{bind("fd00::1")},
		},
		{
			name:        "an IPv6 Service rejects the IPv4 wildcard",
			families:    []corev1.IPFamily{corev1.IPv6Protocol},
			binds:       []*string{bind("0.0.0.0")},
			wantMessage: []string{`listener "l0"`, "0.0.0.0", "IPv6", "immutable"},
		},
		{
			name:        "an IPv6 Service rejects the unset default bind",
			families:    []corev1.IPFamily{corev1.IPv6Protocol},
			binds:       []*string{nil},
			wantMessage: []string{"0.0.0.0", "IPv6"},
		},
		{
			name:        "an IPv4 Service rejects an IPv6 bind",
			families:    []corev1.IPFamily{corev1.IPv4Protocol},
			binds:       []*string{bind("fd00::1")},
			wantMessage: []string{`listener "l0"`, "fd00::1", "IPv4"},
		},
		{
			name:        "a dual-stack Service rejects an IPv4-only bind",
			families:    []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol},
			binds:       []*string{bind("0.0.0.0")},
			wantMessage: []string{"IPv6"},
		},
		{
			name:     "a dual-stack Service accepts the dual-stack wildcard",
			families: []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol},
			binds:    []*string{bind("::")},
		},
		{
			name:     "an IPv4 Service accepts mixed wildcard and IPv4 binds",
			families: []corev1.IPFamily{corev1.IPv4Protocol},
			binds:    []*string{bind("::"), bind("0.0.0.0")},
		},
		{
			name:        "an IPv6 Service names the offending listener among compatible ones",
			families:    []corev1.IPFamily{corev1.IPv6Protocol},
			binds:       []*string{bind("::"), bind("0.0.0.0")},
			wantMessage: []string{`listener "l1"`, "0.0.0.0", "IPv6"},
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
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec:       corev1.ServiceSpec{IPFamilies: tt.families},
			}

			err := retainedFamilyConflict(proxy, svc)
			if len(tt.wantMessage) == 0 {
				if err != nil {
					t.Fatalf("retainedFamilyConflict = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("retainedFamilyConflict = nil, want a conflict")
			}
			for _, want := range tt.wantMessage {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("conflict %q does not contain %q", err.Error(), want)
				}
			}
		})
	}
}

func TestReconcileProxy_ServiceFamilyConflict(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	scheme := proxyTestScheme(t)

	// A Service the cluster already allocated with IPv6 as its primary
	// family. The fake client stores it where a single-family API
	// server would refuse, so the retained-family path can be exercised
	// directly, including the families the API server cannot allocate.
	proxy := guardTestProxy()
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "gw",
			Namespace:       "default",
			OwnerReferences: []metav1.OwnerReference{controllerRefFor(proxy)},
		},
		Spec: corev1.ServiceSpec{IPFamilies: []corev1.IPFamily{corev1.IPv6Protocol}},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&proxyv1alpha1.Proxy{}).
		WithObjects(proxy, svc).Build()
	r := &ProxyReconciler{Client: cl, Scheme: scheme}
	key := client.ObjectKeyFromObject(proxy)

	// The default bind accepts only IPv4 and cannot serve the retained
	// IPv6 family, so the reconcile must report the conflict in status
	// and publish nothing, rather than update the ConfigMap and
	// Deployment first and fail on the immutable Service afterwards.
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile proxy: %v", err)
	}
	got := &proxyv1alpha1.Proxy{}
	if err := cl.Get(ctx, key, got); err != nil {
		t.Fatalf("get proxy: %v", err)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, proxyv1alpha1.ConditionTypeProgrammed)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "ServiceFamilyConflict" {
		t.Fatalf("Programmed condition = %+v, want False/ServiceFamilyConflict", cond)
	}
	for _, want := range []string{`listener "http"`, "0.0.0.0", "IPv6", "immutable"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("condition message %q does not contain %q", cond.Message, want)
		}
	}
	deployErr := cl.Get(ctx, key, &appsv1.Deployment{})
	if !apierrors.IsNotFound(deployErr) {
		t.Errorf("deployment must not be published while the conflict stands, got %v", deployErr)
	}
	cmErr := cl.Get(ctx, client.ObjectKey{Namespace: "default", Name: "gw-config"}, &corev1.ConfigMap{})
	if !apierrors.IsNotFound(cmErr) {
		t.Errorf("configmap must not be published while the conflict stands, got %v", cmErr)
	}

	// Moving the bind to the dual-stack wildcard is compatible with the
	// retained family: the proxy publishes again and the Service keeps
	// its allocated families.
	wildcard := "::"
	if err := cl.Get(ctx, key, proxy); err != nil {
		t.Fatalf("refresh proxy: %v", err)
	}
	proxy.Spec.Listeners[0].BindAddress = &wildcard
	if err := cl.Update(ctx, proxy); err != nil {
		t.Fatalf("update proxy: %v", err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile proxy after the compatible change: %v", err)
	}
	got = &proxyv1alpha1.Proxy{}
	if err := cl.Get(ctx, key, got); err != nil {
		t.Fatalf("get proxy after the compatible change: %v", err)
	}
	cond = apimeta.FindStatusCondition(got.Status.Conditions, proxyv1alpha1.ConditionTypeProgrammed)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Programmed condition = %+v, want True after the compatible change", cond)
	}
	svcAfter := &corev1.Service{}
	if err := cl.Get(ctx, key, svcAfter); err != nil {
		t.Fatalf("get service after the compatible change: %v", err)
	}
	if !reflect.DeepEqual(svcAfter.Spec.IPFamilies, []corev1.IPFamily{corev1.IPv6Protocol}) {
		t.Errorf("service families = %v, want the retained [IPv6]", svcAfter.Spec.IPFamilies)
	}
}
