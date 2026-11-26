package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	proxyv1alpha1 "github.com/praxis-proxy/gateway-controller/api/proxy/v1alpha1"
)

const (
	proxyStartupTimeout = 3 * time.Minute
	proxyReloadTimeout  = 4 * time.Minute
	proxyRequestTimeout = 10 * time.Second
	proxyPollInterval   = 2 * time.Second
	proxyInitialPort    = int32(18080)
	proxyUpdatedPort    = int32(18081)
)

var _ = Describe("Proxy runtime", func() {
	It("hot reloads filters without restarting and rolls out listener changes", SpecTimeout(10*time.Minute),
		func(ctx context.Context) {
			image := os.Getenv("PRAXIS_E2E_IMAGE")
			Expect(image).NotTo(BeEmpty(), "set PRAXIS_E2E_IMAGE to a verified Praxis runtime image available to the cluster")

			namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "proxy-e2e-"}}
			Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
			DeferCleanup(func(ctx context.Context) error {
				if CurrentSpecReport().Failed() {
					dumpProxyDiagnostics(ctx, namespace.Name)
				}
				return client.IgnoreNotFound(k8sClient.Delete(ctx, namespace))
			})

			proxy := &proxyv1alpha1.Proxy{
				ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: namespace.Name},
				Spec: proxyv1alpha1.ProxySpec{
					Image: image, Replicas: new(int32(1)),
					Listeners: []proxyv1alpha1.PraxisListener{{
						Name: "http", Port: proxyInitialPort, FilterChains: []string{"main"},
					}},
					FilterChains: []proxyv1alpha1.ProxyFilterChain{{
						Name: "main",
						Filters: []proxyv1alpha1.PraxisFilterEntry{{
							Name: "response", InlineFilter: staticResponse("v1"),
						}},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, proxy)).To(Succeed())
			key := client.ObjectKeyFromObject(proxy)

			By("waiting for a running Proxy that serves the first configuration")
			var originalTemplate corev1.PodTemplateSpec
			var originalPods map[types.UID]map[string]int32
			Eventually(func(gom Gomega, ctx context.Context) {
				expectRuntimeReady(gom, ctx, key)
				deployment := &appsv1.Deployment{}
				gom.Expect(k8sClient.Get(ctx, key, deployment)).To(Succeed())
				originalTemplate = deployment.Spec.Template
				config := &corev1.ConfigMap{}
				gom.Expect(k8sClient.Get(ctx, client.ObjectKey{
					Namespace: key.Namespace, Name: key.Name + "-config",
				}, config)).To(Succeed())
				gom.Expect(config.Data["config.yaml"]).To(ContainSubstring("v1"))
				var err error
				originalPods, err = proxyPodRestarts(ctx, key.Namespace)
				gom.Expect(err).NotTo(HaveOccurred())
				gom.Expect(originalPods).To(HaveLen(1))
				body, err := proxyServiceBody(ctx, key, proxyInitialPort)
				gom.Expect(err).NotTo(HaveOccurred())
				gom.Expect(body).To(Equal("v1"))
			}).WithContext(ctx).WithTimeout(proxyStartupTimeout).WithPolling(proxyPollInterval).Should(Succeed())

			By("updating only the filter response and observing live traffic without a pod restart")
			updateProxy(ctx, key, func(spec *proxyv1alpha1.ProxySpec) {
				spec.FilterChains[0].Filters[0].InlineFilter = staticResponse("v2")
			})
			Eventually(func(gom Gomega, ctx context.Context) {
				// Request the Service first: reading the Deployment
				// template and pod state afterwards means an unwanted
				// rollout that starts during the reload window is
				// still caught by the assertions below, rather than
				// the reads racing ahead of a rollout the response
				// has not yet reflected.
				body, err := proxyServiceBody(ctx, key, proxyInitialPort)
				gom.Expect(err).NotTo(HaveOccurred())
				gom.Expect(body).To(Equal("v2"))
				deployment := &appsv1.Deployment{}
				gom.Expect(k8sClient.Get(ctx, key, deployment)).To(Succeed())
				gom.Expect(deployment.Spec.Template).To(Equal(originalTemplate))
				pods, err := proxyPodRestarts(ctx, key.Namespace)
				gom.Expect(err).NotTo(HaveOccurred())
				gom.Expect(pods).To(Equal(originalPods), "filter changes must preserve pod UIDs and container restart counts")
			}).WithContext(ctx).WithTimeout(proxyReloadTimeout).WithPolling(proxyPollInterval).Should(Succeed())

			By("changing the listener port and waiting for replacement pods serving the new Service port")
			updateProxy(ctx, key, func(spec *proxyv1alpha1.ProxySpec) {
				spec.Listeners[0].Port = proxyUpdatedPort
			})
			Eventually(func(gom Gomega, ctx context.Context) {
				expectRuntimeReady(gom, ctx, key)
				deployment := &appsv1.Deployment{}
				gom.Expect(k8sClient.Get(ctx, key, deployment)).To(Succeed())
				gom.Expect(deployment.Spec.Template).NotTo(Equal(originalTemplate))
				pods, err := proxyPodRestarts(ctx, key.Namespace)
				gom.Expect(err).NotTo(HaveOccurred())
				gom.Expect(pods).To(HaveLen(1))
				for uid := range originalPods {
					gom.Expect(pods).NotTo(HaveKey(uid), "listener changes must replace the original pod")
				}
				service := &corev1.Service{}
				gom.Expect(k8sClient.Get(ctx, key, service)).To(Succeed())
				gom.Expect(service.Spec.Ports).To(HaveLen(1))
				gom.Expect(service.Spec.Ports[0].Port).To(Equal(proxyUpdatedPort))
				body, err := proxyServiceBody(ctx, key, proxyUpdatedPort)
				gom.Expect(err).NotTo(HaveOccurred())
				gom.Expect(body).To(Equal("v2"))
			}).WithContext(ctx).WithTimeout(proxyStartupTimeout).WithPolling(proxyPollInterval).Should(Succeed())
		})
})

func staticResponse(body string) *proxyv1alpha1.PraxisInlineFilter {
	config, err := json.Marshal(map[string]any{"status": 200, "body": body})
	Expect(err).NotTo(HaveOccurred())
	return &proxyv1alpha1.PraxisInlineFilter{Type: "static_response", Config: &runtime.RawExtension{Raw: config}}
}

func updateProxy(ctx context.Context, key client.ObjectKey, mutate func(*proxyv1alpha1.ProxySpec)) {
	GinkgoHelper()
	Eventually(func(ctx context.Context) error {
		proxy := &proxyv1alpha1.Proxy{}
		if err := k8sClient.Get(ctx, key, proxy); err != nil {
			return err
		}
		mutate(&proxy.Spec)
		return k8sClient.Update(ctx, proxy)
	}).WithContext(ctx).WithTimeout(proxyRequestTimeout).WithPolling(proxyPollInterval).Should(Succeed())
}

func expectRuntimeReady(gom Gomega, ctx context.Context, key client.ObjectKey) {
	GinkgoHelper()
	proxy := &proxyv1alpha1.Proxy{}
	gom.Expect(k8sClient.Get(ctx, key, proxy)).To(Succeed())
	ready := meta.FindStatusCondition(proxy.Status.Conditions, proxyv1alpha1.ConditionTypeReady)
	gom.Expect(ready).NotTo(BeNil())
	gom.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
	gom.Expect(ready.ObservedGeneration).To(Equal(proxy.Generation))
	deployment := &appsv1.Deployment{}
	gom.Expect(k8sClient.Get(ctx, key, deployment)).To(Succeed())
	gom.Expect(deployment.Status.ObservedGeneration).To(Equal(deployment.Generation))
	gom.Expect(deployment.Status.UpdatedReplicas).To(Equal(int32(1)))
	gom.Expect(deployment.Status.ReadyReplicas).To(Equal(int32(1)))
}

// proxyServiceBody uses the API server's Service proxy to reach a selected
// backend. It tests Service routing and the runtime without another client image;
// it does not exercise kube-proxy's ClusterIP packet forwarding.
func proxyServiceBody(ctx context.Context, key client.ObjectKey, port int32) (string, error) {
	requestCtx, cancel := context.WithTimeout(ctx, proxyRequestTimeout)
	defer cancel()
	body, err := coreClient.Services(key.Namespace).
		ProxyGet("http", key.Name, strconv.FormatInt(int64(port), 10), "/", nil).DoRaw(requestCtx)
	if err != nil {
		return "", fmt.Errorf("request Service %s:%d: %w", key, port, err)
	}
	return string(body), nil
}

func proxyPodRestarts(ctx context.Context, namespace string) (map[types.UID]map[string]int32, error) {
	pods := &corev1.PodList{}
	if err := k8sClient.List(ctx, pods, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list Proxy pods: %w", err)
	}
	restarts := make(map[types.UID]map[string]int32)
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp != nil {
			continue
		}
		containers := make(map[string]int32)
		for _, container := range pod.Status.ContainerStatuses {
			containers[container.Name] = container.RestartCount
		}
		if len(containers) == 0 {
			return nil, fmt.Errorf("pod %s has no container status", pod.Name)
		}
		restarts[pod.UID] = containers
	}
	return restarts, nil
}

// dumpProxyDiagnostics runs before namespace cleanup without adding assertions
// that could replace the original test failure.
func dumpProxyDiagnostics(ctx context.Context, namespace string) {
	const (
		maxPods        = 3
		maxStatusBytes = 4096
		maxLogBytes    = int64(16384)
		tailLines      = int64(50)
	)
	diagnosticCtx, cancel := context.WithTimeout(ctx, proxyRequestTimeout)
	defer cancel()
	pods := &corev1.PodList{}
	if err := k8sClient.List(diagnosticCtx, pods, client.InNamespace(namespace)); err != nil {
		GinkgoWriter.Printf("Proxy diagnostics: list pods: %v\n", err)
		return
	}
	for _, pod := range pods.Items[:min(len(pods.Items), maxPods)] {
		status, err := json.Marshal(pod.Status)
		if err != nil {
			GinkgoWriter.Printf("Pod %s: encode status: %v\n", pod.Name, err)
		} else {
			GinkgoWriter.Printf("Pod %s status: %s\n", pod.Name, status[:min(len(status), maxStatusBytes)])
		}
		logs, err := coreClient.Pods(namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			Container: "praxis", TailLines: new(tailLines), LimitBytes: new(maxLogBytes),
		}).DoRaw(diagnosticCtx)
		if err != nil {
			GinkgoWriter.Printf("Pod %s: read Praxis logs: %v\n", pod.Name, err)
			continue
		}
		GinkgoWriter.Printf("Pod %s Praxis logs (last %d lines):\n%s\n", pod.Name, tailLines, logs)
	}
}
