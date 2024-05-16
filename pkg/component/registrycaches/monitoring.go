// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package registrycaches

import (
	"context"
	_ "embed"
	"fmt"
	"strconv"

	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	kubeapiserverconstants "github.com/gardener/gardener/pkg/component/kubernetes/apiserver/constants"
	monitoringutils "github.com/gardener/gardener/pkg/component/observability/monitoring/utils"
	"github.com/gardener/gardener/pkg/controllerutils"
	"github.com/gardener/gardener/pkg/utils"
	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	monitoringv1alpha1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	scrapeConfigYAML = `- job_name: registry-cache-metrics
  scheme: https
  tls_config:
    ca_file: /etc/prometheus/seed/ca.crt
  authorization:
    type: Bearer
    credentials_file: /var/run/secrets/gardener.cloud/shoot/token/token
  honor_labels: false
  kubernetes_sd_configs:
  - role: pod
    api_server: https://` + v1beta1constants.DeploymentNameKubeAPIServer + `:` + strconv.Itoa(kubeapiserverconstants.Port) + `
    namespaces:
      names: [ kube-system ]
    tls_config:
      ca_file: /etc/prometheus/seed/ca.crt
    authorization:
      type: Bearer
      credentials_file: /var/run/secrets/gardener.cloud/shoot/token/token
  relabel_configs:
  - source_labels: [__meta_kubernetes_pod_label_upstream_host, __meta_kubernetes_pod_container_port_name]
    action: keep
    regex: (.+);debug
  - action: labelmap
    regex: __meta_kubernetes_pod_label_(.+)
  - target_label: __address__
    action: replace
    replacement: ` + v1beta1constants.DeploymentNameKubeAPIServer + `:` + strconv.Itoa(kubeapiserverconstants.Port) + `
  - source_labels: [__meta_kubernetes_pod_name, __meta_kubernetes_pod_container_port_number]
    action: replace
    target_label: __metrics_path__
    regex: (.+);(.+)
    replacement: /api/v1/namespaces/kube-system/pods/${1}:${2}/proxy/metrics
  metric_relabel_configs:
  - source_labels: [ __name__ ]
    regex: registry_proxy_.+
    action: keep
`
	//go:embed alerting-rules/registry-cache.rules.yaml
	monitoringAlertingRules string
	//go:embed monitoring/dashboard.json
	dashboard string
)

func (r *registryCaches) alertingRules() string {
	return fmt.Sprintf("registry-cache.rules.yaml: |\n  %s\n", utils.Indent(monitoringAlertingRules, 2))
}

func (r *registryCaches) dashboard() string {
	return fmt.Sprintf("registry-cache.dashboard.json: '%s'", dashboard)
}

func (r *registryCaches) scrapeConfig() string {
	return scrapeConfigYAML
}

func (r *registryCaches) deployMonitoringConfig(ctx context.Context) error {
	// TODO(dimitar-kostadinov): Delete this if-condition after August 2024.
	if r.client.Get(ctx, client.ObjectKey{Name: "prometheus-shoot", Namespace: r.namespace}, &appsv1.StatefulSet{}) == nil {
		if err := kutil.DeleteObject(ctx, r.client, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "extension-registry-cache-monitoring", Namespace: r.namespace}}); err != nil {
			return fmt.Errorf("failed deleting %s ConfigMap: %w", "extension-registry-cache-monitoring", err)
		}

		configMapDashboards := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "registry-cache-dashboards", Namespace: r.namespace}}
		if _, err := controllerutils.GetAndCreateOrMergePatch(ctx, r.client, configMapDashboards, func() error {
			metav1.SetMetaDataLabel(&configMapDashboards.ObjectMeta, "component", "registry-cache")
			metav1.SetMetaDataLabel(&configMapDashboards.ObjectMeta, "dashboard.monitoring.gardener.cloud/shoot", "true")
			configMapDashboards.Data = map[string]string{"registry-cache.dashboard.json": dashboard}
			return nil
		}); err != nil {
			return err
		}

		prometheusRule := &monitoringv1.PrometheusRule{ObjectMeta: monitoringutils.ConfigObjectMeta("registry-cache", r.namespace, "shoot")}
		if _, err := controllerutils.GetAndCreateOrMergePatch(ctx, r.client, prometheusRule, func() error {
			metav1.SetMetaDataLabel(&prometheusRule.ObjectMeta, "component", "registry-cache")
			metav1.SetMetaDataLabel(&prometheusRule.ObjectMeta, "prometheus", "shoot")
			prometheusRule.Spec = monitoringv1.PrometheusRuleSpec{
				Groups: []monitoringv1.RuleGroup{{
					Name: "registry-cache.rules",
					Rules: []monitoringv1.Rule{
						{
							Alert: "RegistryCachePersistentVolumeUsageCritical",
							Expr: intstr.FromString(`100 * (
	 kubelet_volume_stats_available_bytes{persistentvolumeclaim=~"^cache-volume-registry-.+$"}
	   /
	 kubelet_volume_stats_capacity_bytes{persistentvolumeclaim=~"^cache-volume-registry-.+$"}
	) < 5`),
							For: ptr.To(monitoringv1.Duration("1h")),
							Labels: map[string]string{
								"service":    "registry-cache-extension",
								"severity":   "warning",
								"type":       "shoot",
								"visibility": "owner",
							},
							Annotations: map[string]string{
								"description": `The registry-cache PersistentVolume claimed by {{ $labels.persistentvolumeclaim }} is only {{ printf "%0.2f" $value }}% free. When there is no available disk space, no new images will be cached. However, image pull operations are not affected.`,
								"summary":     "Registry cache PersistentVolume almost full.",
							},
						},
						{
							Alert: "RegistryCachePersistentVolumeFullInFourDays",
							Expr: intstr.FromString(`100 * (
	 kubelet_volume_stats_available_bytes{persistentvolumeclaim=~"^cache-volume-registry-.+$"}
	   /
	 kubelet_volume_stats_capacity_bytes{persistentvolumeclaim=~"^cache-volume-registry-.+$"}
	) < 15
	and
	predict_linear(kubelet_volume_stats_available_bytes{persistentvolumeclaim=~"^cache-volume-registry-.+$"}[30m], 4 * 24 * 3600) <= 0`),
							For: ptr.To(monitoringv1.Duration("1h")),
							Labels: map[string]string{
								"service":    "registry-cache-extension",
								"severity":   "warning",
								"type":       "shoot",
								"visibility": "owner",
							},
							Annotations: map[string]string{
								"description": `Based on recent sampling, the registry cache PersistentVolume claimed by {{ $labels.persistentvolumeclaim }} is expected to fill up within four days. Currently {{ printf "%0.2f" $value }}% is available.`,
								"summary":     "Registry cache PersistentVolume will be full in four days.",
							},
						},
						// We rely on the implicit contract that recording rules in format "shoot:(.+):(.+)" will be
						// automatically federated to the aggregate prometheus and then to the garden-prometheus.
						// Ref https://github.com/gardener/gardener/blob/v1.90.0/pkg/component/observability/monitoring/prometheus/aggregate/servicemonitors.go#L45
						{
							Record: "shoot:registry_proxy_pushed_bytes_total:sum",
							Expr:   intstr.FromString("sum by (upstream_host) (rate(registry_proxy_pushed_bytes_total[5m]))"),
						},
						{
							Record: "shoot:registry_proxy_pulled_bytes_total:sum",
							Expr:   intstr.FromString("sum by (upstream_host) (rate(registry_proxy_pulled_bytes_total[5m]))"),
						},
					},
				}},
			}
			return nil
		}); err != nil {
			return err
		}

		scrapeConfig := &monitoringv1alpha1.ScrapeConfig{ObjectMeta: monitoringutils.ConfigObjectMeta("registry-cache", r.namespace, "shoot")}
		if _, err := controllerutils.GetAndCreateOrMergePatch(ctx, r.client, scrapeConfig, func() error {
			metav1.SetMetaDataLabel(&scrapeConfig.ObjectMeta, "component", "registry-cache")
			metav1.SetMetaDataLabel(&scrapeConfig.ObjectMeta, "prometheus", "shoot")
			scrapeConfig.Spec = monitoringv1alpha1.ScrapeConfigSpec{
				HonorLabels:   ptr.To(false),
				ScrapeTimeout: ptr.To(monitoringv1.Duration("10s")),
				Scheme:        ptr.To("HTTPS"),
				// This is needed because the kubelets' certificates are not are generated for a specific pod IP
				TLSConfig: &monitoringv1.SafeTLSConfig{InsecureSkipVerify: true},
				Authorization: &monitoringv1.SafeAuthorization{Credentials: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "shoot-access-prometheus-shoot"},
					Key:                  "token",
				}},
				KubernetesSDConfigs: []monitoringv1alpha1.KubernetesSDConfig{{
					APIServer:  ptr.To("https://" + v1beta1constants.DeploymentNameKubeAPIServer + ":" + strconv.Itoa(kubeapiserverconstants.Port)),
					Role:       "endpoints",
					Namespaces: &monitoringv1alpha1.NamespaceDiscovery{Names: []string{metav1.NamespaceSystem}},
					Authorization: &monitoringv1.SafeAuthorization{Credentials: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "shoot-access-prometheus-shoot"},
						Key:                  "token",
					}},
					// This is needed because we do not fetch the correct cluster CA bundle right now
					TLSConfig:       &monitoringv1.SafeTLSConfig{InsecureSkipVerify: true},
					FollowRedirects: ptr.To(true),
				}},
				RelabelConfigs: []*monitoringv1.RelabelConfig{
					{
						Action:      "replace",
						Replacement: "registry-cache-metrics",
						TargetLabel: "job",
					},
					{
						SourceLabels: []monitoringv1.LabelName{"__meta_kubernetes_pod_label_upstream_host", "__meta_kubernetes_pod_container_port_name"},
						Action:       "keep",
						Regex:        `(.+);debug`,
					},
					{
						Action: "labelmap",
						Regex:  `__meta_kubernetes_pod_label_(.+)`,
					},
					{
						TargetLabel: "__address__",
						Action:      "replace",
						Replacement: v1beta1constants.DeploymentNameKubeAPIServer + ":" + strconv.Itoa(kubeapiserverconstants.Port),
					},
					{
						SourceLabels: []monitoringv1.LabelName{"__meta_kubernetes_pod_name", "__meta_kubernetes_pod_container_port_number"},
						Action:       "replace",
						TargetLabel:  "__metrics_path__",
						Regex:        `(.+);(.+)`,
						Replacement:  "/api/v1/namespaces/kube-system/pods/${1}:${2}/proxy/metrics",
					},
				},
				MetricRelabelConfigs: monitoringutils.StandardMetricRelabelConfig("registry_proxy_.+"),
			}
			return nil
		}); err != nil {
			return err
		}

		return nil
	}

	// TODO(dimitar-kostadinov): Delete this, scrapeConfigYAML & monitoringAlertingRules vars and alerting-rules/registry-cache.rules.yaml file after August 2024.
	monitoringConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "extension-registry-cache-monitoring",
			Namespace: r.namespace,
		},
	}
	_, err := controllerutils.GetAndCreateOrMergePatch(ctx, r.client, monitoringConfigMap, func() error {
		metav1.SetMetaDataLabel(&monitoringConfigMap.ObjectMeta, v1beta1constants.LabelExtensionConfiguration, v1beta1constants.LabelMonitoring)

		monitoringConfigMap.Data = map[string]string{
			v1beta1constants.PrometheusConfigMapAlertingRules:  r.alertingRules(),
			v1beta1constants.PrometheusConfigMapScrapeConfig:   r.scrapeConfig(),
			v1beta1constants.PlutonoConfigMapOperatorDashboard: r.dashboard(),
		}

		return nil
	})

	return err
}
