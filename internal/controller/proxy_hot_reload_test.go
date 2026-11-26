package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"

	proxyv1alpha1 "github.com/praxis-proxy/gateway-controller/api/proxy/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestReconcileProxy_RolloutTriggers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		change        func(*proxyv1alpha1.Proxy)
		wantRollout   bool
		wantNewConfig bool
	}{
		{
			name: "filter configuration reloads without a rollout",
			change: func(proxy *proxyv1alpha1.Proxy) {
				proxy.Spec.FilterChains[0].Filters[0].InlineFilter.Config = &runtime.RawExtension{
					Raw: []byte(`{"requests_per_second":25}`),
				}
			},
			wantNewConfig: true,
		},
		{
			name: "filter chain readiness name collision reloads without a rollout",
			change: func(proxy *proxyv1alpha1.Proxy) {
				proxy.Spec.FilterChains = append(proxy.Spec.FilterChains, proxyv1alpha1.ProxyFilterChain{
					Name: "readiness",
					Filters: []proxyv1alpha1.PraxisFilterEntry{{
						Name: "id", InlineFilter: &proxyv1alpha1.PraxisInlineFilter{Type: "request_id"},
					}},
				})
			},
			wantNewConfig: true,
		},
		{
			name: "scaling changes replicas without a rollout",
			change: func(proxy *proxyv1alpha1.Proxy) {
				proxy.Spec.Replicas = new(int32(3))
			},
		},
		{
			name: "listener port change rolls pods",
			change: func(proxy *proxyv1alpha1.Proxy) {
				proxy.Spec.Listeners[0].Port = 9090
			},
			wantRollout:   true,
			wantNewConfig: true,
		},
		{
			name: "listener bind change rolls pods",
			change: func(proxy *proxyv1alpha1.Proxy) {
				proxy.Spec.Listeners[0].BindAddress = new("127.0.0.1")
			},
			wantRollout:   true,
			wantNewConfig: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			scheme := proxyTestScheme(t)
			proxy := guardTestProxy()
			cl := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&proxyv1alpha1.Proxy{}).
				WithObjects(proxy).Build()
			r := &ProxyReconciler{Client: cl, Scheme: scheme}
			key := client.ObjectKeyFromObject(proxy)
			reconcile := func() {
				t.Helper()
				if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
					t.Fatalf("reconcile proxy: %v", err)
				}
			}
			reconcile()
			var before appsv1.Deployment
			if err := cl.Get(ctx, key, &before); err != nil {
				t.Fatalf("get original deployment: %v", err)
			}
			originalConfig := fetchConfigMap(t, cl, configMapName(proxy)).Data[configFileName]

			if err := cl.Get(ctx, key, proxy); err != nil {
				t.Fatalf("get proxy: %v", err)
			}
			tt.change(proxy)
			if err := cl.Update(ctx, proxy); err != nil {
				t.Fatalf("update proxy: %v", err)
			}
			reconcile()
			var after appsv1.Deployment
			if err := cl.Get(ctx, key, &after); err != nil {
				t.Fatalf("get updated deployment: %v", err)
			}
			if changed := !reflect.DeepEqual(before.Spec.Template, after.Spec.Template); changed != tt.wantRollout {
				t.Errorf("pod template changed = %t, want %t", changed, tt.wantRollout)
			}
			newConfig := fetchConfigMap(t, cl, configMapName(proxy)).Data[configFileName]
			if changed := newConfig != originalConfig; changed != tt.wantNewConfig {
				t.Errorf("published config changed = %t, want %t", changed, tt.wantNewConfig)
			}
			if proxy.Spec.Replicas != nil && (after.Spec.Replicas == nil || *after.Spec.Replicas != *proxy.Spec.Replicas) {
				t.Errorf("deployment replica count does not match the requested scale")
			}
		})
	}
}

func TestBuildPodTemplate_ConfigMountReceivesUpdates(t *testing.T) {
	t.Parallel()
	proxy := guardTestProxy()
	template := buildPodTemplate(proxy, "revision")
	for _, volume := range template.Spec.Volumes {
		if volume.Name != configVolumeName {
			continue
		}
		if volume.ConfigMap == nil {
			t.Fatal("config volume must project a ConfigMap")
		}
		if volume.ConfigMap.Name != configMapName(proxy) {
			t.Errorf("config source = %q, want live publication %q", volume.ConfigMap.Name, configMapName(proxy))
		}
		for _, container := range template.Spec.Containers {
			for _, mount := range container.VolumeMounts {
				if mount.Name == configVolumeName {
					if mount.SubPath != "" || mount.SubPathExpr != "" {
						t.Error("config mount uses subPath, preventing ConfigMap update propagation")
					}
					if mount.MountPath != configMountPath {
						t.Errorf("config mount path = %q, want directory %q", mount.MountPath, configMountPath)
					}
					return
				}
			}
		}
	}
	t.Fatal("pod template has no configuration mount")
}

func TestBuildPodTemplate_ConfigAndTLSMounts(t *testing.T) {
	t.Parallel()
	proxy := guardTestProxy()
	proxy.Spec.Listeners[0].TLS = &proxyv1alpha1.PraxisListenerTLS{
		CertificateRef: proxyv1alpha1.PraxisLocalReference{Name: "gateway-tls"},
	}
	container := buildPodTemplate(proxy, "revision").Spec.Containers[0]
	wantArgs := []string{"--config", "/etc/praxis/config/config.yaml"}
	if !reflect.DeepEqual(container.Args, wantArgs) {
		t.Errorf("args = %v, want %v", container.Args, wantArgs)
	}
	for i, mount := range container.VolumeMounts {
		for j, other := range container.VolumeMounts {
			if i != j && strings.HasPrefix(other.MountPath, mount.MountPath+"/") {
				t.Errorf("mount %q overlaps nested mount %q", mount.MountPath, other.MountPath)
			}
		}
	}
}
