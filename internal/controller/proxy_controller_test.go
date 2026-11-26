package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	proxyv1alpha1 "github.com/praxis-proxy/gateway-controller/api/proxy/v1alpha1"
)

func proxyTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := proxyv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("register proxy types: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("register core types: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("register apps types: %v", err)
	}
	return scheme
}

func guardTestProxy() *proxyv1alpha1.Proxy {
	return &proxyv1alpha1.Proxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gw",
			Namespace: "default",
			UID:       "proxy-uid",
		},
		Spec: proxyv1alpha1.ProxySpec{
			Image: "ghcr.io/praxis-proxy/praxis:latest",
			FilterChains: []proxyv1alpha1.ProxyFilterChain{{
				Name: "main",
				Filters: []proxyv1alpha1.PraxisFilterEntry{{
					Name:         "rl",
					InlineFilter: &proxyv1alpha1.PraxisInlineFilter{Type: "rate_limit"},
				}},
			}},
			Listeners: []proxyv1alpha1.PraxisListener{{
				Name:         "http",
				Port:         8080,
				FilterChains: []string{"main"},
			}},
		},
	}
}

func controllerRefFor(proxy *proxyv1alpha1.Proxy) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "praxis.io/v1alpha1",
		Kind:       "Proxy",
		Name:       proxy.Name,
		UID:        proxy.UID,
		Controller: new(true),
	}
}

func foreignControllerRef() metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "someone-elses",
		UID:        "foreign-uid",
		Controller: new(true),
	}
}

func unownedConfigMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "gw-config",
			Namespace:         "default",
			UID:               "cm-uid",
			CreationTimestamp: metav1.Now(),
		},
		Data: map[string]string{configFileName: "user-managed"},
	}
}

// TestReconcileResources_RefuseUnownedObjects verifies that the reconciler
// never adopts or overwrites existing objects it does not control: an
// object with no controller owner, or one controlled by another
// controller, must produce a conflict error and remain untouched.
func TestReconcileResources_RefuseUnownedObjects(t *testing.T) {
	t.Parallel()

	scheme := proxyTestScheme(t)
	proxy := guardTestProxy()

	tests := []struct {
		name      string
		existing  client.Object
		reconcile func(ctx context.Context, r *ProxyReconciler) error
		key       types.NamespacedName
		fetch     func() client.Object
		check     func(t *testing.T, got client.Object)
	}{
		{
			name:     "configmap without controller owner",
			existing: unownedConfigMap(),
			reconcile: func(ctx context.Context, r *ProxyReconciler) error {
				return r.reconcileConfigMap(ctx, proxy, "rendered-config")
			},
			key:   types.NamespacedName{Namespace: "default", Name: "gw-config"},
			fetch: func() client.Object { return &corev1.ConfigMap{} },
			check: func(t *testing.T, got client.Object) {
				cm := got.(*corev1.ConfigMap)
				if cm.Data[configFileName] != "user-managed" {
					t.Errorf("unowned ConfigMap was overwritten: %q", cm.Data[configFileName])
				}
				if owner := metav1.GetControllerOf(cm); owner != nil {
					t.Errorf("unowned ConfigMap was adopted by %q", owner.Name)
				}
			},
		},
		{
			name:     "configmap controlled by another controller",
			existing: withForeignOwner(unownedConfigMap()),
			reconcile: func(ctx context.Context, r *ProxyReconciler) error {
				return r.reconcileConfigMap(ctx, proxy, "rendered-config")
			},
			key:   types.NamespacedName{Namespace: "default", Name: "gw-config"},
			fetch: func() client.Object { return &corev1.ConfigMap{} },
			check: func(t *testing.T, got client.Object) {
				cm := got.(*corev1.ConfigMap)
				if cm.Data[configFileName] != "user-managed" {
					t.Errorf("foreign-owned ConfigMap was overwritten: %q", cm.Data[configFileName])
				}
				if owner := metav1.GetControllerOf(cm); owner == nil || owner.UID != "foreign-uid" {
					t.Errorf("foreign controller owner was replaced: %+v", owner)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Seed a per-subtest copy: the fake client assigns resource
			// versions to seeded objects during Build, and subtests run
			// in parallel.
			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(proxy.DeepCopy(), tt.existing).
				Build()
			r := &ProxyReconciler{Client: cl, Scheme: scheme}

			err := tt.reconcile(context.Background(), r)
			if !apierrors.IsConflict(err) {
				t.Fatalf("expected conflict error, got: %v", err)
			}

			got := tt.fetch()
			if err := cl.Get(context.Background(), tt.key, got); err != nil {
				t.Fatalf("fetch object after reconcile: %v", err)
			}
			tt.check(t, got)
		})
	}
}

// TestReconcileResources_MutateOwnedAndCreateObjects verifies the guard
// does not interfere with the intended flows: objects already controlled
// by the proxy are updated, and absent objects are created.
func TestReconcileResources_MutateOwnedAndCreateObjects(t *testing.T) {
	t.Parallel()

	scheme := proxyTestScheme(t)
	proxy := guardTestProxy()

	tests := []struct {
		name      string
		existing  client.Object // nil: object absent
		reconcile func(ctx context.Context, r *ProxyReconciler) error
		key       types.NamespacedName
		fetch     func() client.Object
		check     func(t *testing.T, got client.Object)
	}{
		{
			name:     "configmap absent is created",
			existing: nil,
			reconcile: func(ctx context.Context, r *ProxyReconciler) error {
				return r.reconcileConfigMap(ctx, proxy, "rendered-config")
			},
			key:   types.NamespacedName{Namespace: "default", Name: "gw-config"},
			fetch: func() client.Object { return &corev1.ConfigMap{} },
			check: func(t *testing.T, got client.Object) {
				cm := got.(*corev1.ConfigMap)
				if cm.Data[configFileName] != "rendered-config" {
					t.Errorf("config data: %q", cm.Data[configFileName])
				}
				if owner := metav1.GetControllerOf(cm); owner == nil || owner.UID != proxy.UID {
					t.Errorf("controller owner: %+v", owner)
				}
			},
		},
		{
			name:     "configmap owned by proxy is updated",
			existing: withControllerOwner(unownedConfigMap(), proxy),
			reconcile: func(ctx context.Context, r *ProxyReconciler) error {
				return r.reconcileConfigMap(ctx, proxy, "rendered-config")
			},
			key:   types.NamespacedName{Namespace: "default", Name: "gw-config"},
			fetch: func() client.Object { return &corev1.ConfigMap{} },
			check: func(t *testing.T, got client.Object) {
				cm := got.(*corev1.ConfigMap)
				if cm.Data[configFileName] != "rendered-config" {
					t.Errorf("config data: %q", cm.Data[configFileName])
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(proxy.DeepCopy())
			if tt.existing != nil {
				builder = builder.WithObjects(tt.existing)
			}
			cl := builder.Build()
			r := &ProxyReconciler{Client: cl, Scheme: scheme}

			if err := tt.reconcile(context.Background(), r); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			got := tt.fetch()
			if err := cl.Get(context.Background(), tt.key, got); err != nil {
				t.Fatalf("fetch object after reconcile: %v", err)
			}
			tt.check(t, got)
		})
	}
}

func withForeignOwner(obj client.Object) client.Object {
	obj.SetOwnerReferences([]metav1.OwnerReference{foreignControllerRef()})
	return obj
}

func withControllerOwner(obj client.Object, proxy *proxyv1alpha1.Proxy) client.Object {
	obj.SetOwnerReferences([]metav1.OwnerReference{controllerRefFor(proxy)})
	return obj
}

func fetchConfigMap(t *testing.T, cl client.Client, name string) *corev1.ConfigMap {
	t.Helper()
	cm := &corev1.ConfigMap{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, cm); err != nil {
		t.Fatalf("get configmap %q: %v", name, err)
	}
	return cm
}

func TestReconcileConfigMap_LiveUpdates(t *testing.T) {
	t.Parallel()
	proxy := guardTestProxy()
	scheme := proxyTestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(proxy).Build()
	r := &ProxyReconciler{Client: cl, Scheme: scheme}
	ctx := context.Background()
	for _, config := range []string{"config-v1", "config-v2", "config-v2"} {
		if err := r.reconcileConfigMap(ctx, proxy, config); err != nil {
			t.Fatalf("publish config: %v", err)
		}
		cm := fetchConfigMap(t, cl, configMapName(proxy))
		if len(cm.Data) != 1 || cm.Data[configFileName] != config {
			t.Errorf("published config = %v, want only %q", cm.Data, config)
		}
		if owner := metav1.GetControllerOf(cm); owner == nil || owner.UID != proxy.UID {
			t.Errorf("config controller owner = %+v", owner)
		}
	}
	var maps corev1.ConfigMapList
	if err := cl.List(ctx, &maps); err != nil {
		t.Fatal(err)
	}
	if len(maps.Items) != 1 {
		t.Errorf("live publication created %d ConfigMaps, want 1", len(maps.Items))
	}
}
