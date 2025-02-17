package clusterconfig

import (
	"context"
	"fmt"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	configv1client "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog/v2"

	"github.com/openshift/insights-operator/pkg/record"
	"github.com/openshift/insights-operator/pkg/utils/anonymize"
	"github.com/openshift/insights-operator/pkg/utils/check"
	"github.com/openshift/insights-operator/pkg/utils/marshal"
)

// GatherClusterVersion Collects the `ClusterVersion` (including the cluster ID) with the name
// 'version' and its resources.
//
// ### API Reference
// - https://github.com/openshift/client-go/blob/master/config/clientset/versioned/typed/config/v1/clusterversion.go#L50
// - https://docs.openshift.com/container-platform/4.3/rest_api/index.html#clusterversion-v1config-openshift-io
//
// ### Sample data
// - docs/insights-archive-sample/config/version.json
// - docs/insights-archive-sample/config/pod
// - docs/insights-archive-sample/events/
// - docs/insights-archive-sample/config/id
//
// ### Location in archive
// | Version   | Path														|
// | --------- | ---------------------------------------------------------- |
// | >= 4.2.0  | config/version.json										|
// | >= 4.2.0  | config/id													|
// | >= 4.8.2  | config/pod/openshift-cluster-version/version.json			|
// | >= 4.8.2  | events/openshift-cluster-version.json						|
//
// ### Config ID
// `clusterconfig/version`
//
// ### Released version
// - 4.2.0
//
// ### Backported versions
// None
//
// ### Changes
// None
func (g *Gatherer) GatherClusterVersion(ctx context.Context) ([]record.Record, []error) {
	gatherConfigClient, err := configv1client.NewForConfig(g.gatherKubeConfig)
	if err != nil {
		return nil, []error{err}
	}

	gatherKubeClient, err := kubernetes.NewForConfig(g.gatherProtoKubeConfig)
	if err != nil {
		return nil, []error{err}
	}

	return getClusterVersion(ctx, gatherConfigClient, gatherKubeClient.CoreV1(), g.interval)
}

func getClusterVersion(ctx context.Context,
	configClient configv1client.ConfigV1Interface,
	coreClient corev1client.CoreV1Interface,
	interval time.Duration) ([]record.Record, []error) {
	config, err := configClient.ClusterVersions().Get(ctx, "version", metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, []error{err}
	}

	records := []record.Record{
		{Name: "config/version", Item: record.ResourceMarshaller{Resource: anonymizeClusterVersion(config)}},
	}

	if config.Spec.ClusterID != "" {
		records = append(records, record.Record{Name: "config/id", Item: marshal.Raw{Str: string(config.Spec.ClusterID)}})
	}

	namespace := "openshift-cluster-version"
	now := time.Now()
	var unhealthyPods []*corev1.Pod

	pods, err := coreClient.Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.V(2).Infof("Unable to find pods in namespace %s for cluster-version operator", namespace)
		return records, nil
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		anonymize.SensitiveEnvVars(pod.Spec.Containers)

		records = append(records, record.Record{
			Name: fmt.Sprintf("config/pod/%s/%s", pod.Namespace, pod.Name),
			Item: record.ResourceMarshaller{Resource: pod},
		})

		if check.IsHealthyPod(pod, now) {
			continue
		}

		unhealthyPods = append(unhealthyPods, pod)
	}

	// Exit early if no unhealthy pods found
	if len(unhealthyPods) == 0 {
		return records, nil
	}
	klog.V(2).Infof("Found %d unhealthy pods in %s", len(unhealthyPods), namespace)

	namespaceRecords, err := gatherNamespaceEvents(ctx, coreClient, namespace, interval)
	if err != nil {
		klog.V(2).Infof("Unable to collect events for namespace %q: %#v", namespace, err)
	}
	records = append(records, namespaceRecords...)

	return records, nil
}

func anonymizeClusterVersion(version *configv1.ClusterVersion) *configv1.ClusterVersion {
	version.Spec.Upstream = configv1.URL(anonymize.URL(string(version.Spec.Upstream)))
	return version
}
