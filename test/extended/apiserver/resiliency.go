package apiserver

import (
	"context"
	"fmt"
	"github.com/openshift/origin/test/extended/operators"
	"io/ioutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"net/http"
	"strings"
	"time"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	configv1 "github.com/openshift/api/config/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	types "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"k8s.io/kubernetes/test/e2e/framework"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"

	"github.com/openshift/origin/test/extended/scheme"
	"github.com/openshift/origin/test/extended/single_node"
	exutil "github.com/openshift/origin/test/extended/util"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

var _ = ginkgo.Describe("[Conformance][sig-sno][Serial] Cluster", func() {
	f := framework.NewDefaultFramework("cluster-resiliency")
	f.SkipNamespaceCreation = true
	f.SkipPrivilegedPSPBinding = true

	oc := exutil.NewCLIWithoutNamespace("cluster-resiliency")

	ginkgo.It("should allow a fast rollout of kube-apiserver", func() {
		controlPlaneTopology, _ := single_node.GetTopologies(f)

		if controlPlaneTopology != configv1.SingleReplicaTopologyMode {
			e2eskipper.Skipf("Test is only relevant for single replica topologies")
		}

		config, err := framework.LoadConfig()
		framework.ExpectNoError(err)

		setRESTConfigDefaults(config)
		restClient, err := rest.RESTClientFor(config)
		framework.ExpectNoError(err)

		httpClient := restClient.Client

		ginkgo.By("Making sure no previous rollout is in progress")
		clusterApiServer, err := oc.AdminOperatorClient().OperatorV1().KubeAPIServers().Get(context.Background(), "cluster", metav1.GetOptions{})
		framework.ExpectNoError(err)
		gomega.Expect(clusterApiServer.Status.NodeStatuses[0].TargetRevision).To(gomega.Equal(int32(0)))

		ginkgo.By("Initialize pods restart count")
		restartingContainers := make(map[operators.ContainerName]int)
		c, err := e2e.LoadClientset()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		// This will just load the restartingContainers map with the current restart count
		// The current restart count is the baseline for validating that there was no restarts during the API rollout
		_ = GetRestartedPods(c, restartingContainers)

		ginkgo.By("Forcing API rollout")
		forceApiRollout(oc)

		ginkgo.By("Expecting API to become unavailable")
		wait.PollImmediate(time.Second, time.Minute, func() (bool, error) {
			ready, _, err := isApiReady(config, httpClient)
			return !ready, err
		})

		start := time.Now()

		ginkgo.By("Expecting API to become ready")
		wait.PollImmediate(time.Second, time.Minute, func() (bool, error) {
			ready, _, _ := isApiReady(config, httpClient)
			return ready, nil
		})

		end := time.Now()

		ginkgo.By("Measuring disruption duration time")
		disruptionDuration := end.Sub(start)
		// For more information: https://github.com/openshift/origin/pull/26337/files#r698435488
		gomega.Expect(disruptionDuration).To(gomega.BeNumerically("<", 40*time.Second),
			fmt.Sprintf("Total time of disruption is %v which is more than 40 seconds. ", disruptionDuration)+
				"Actual SLO for this is 60 seconds, yet we want to be notified about major regressions")

		ginkgo.It("with no pods restarts during API disruption", func() {
			names := GetRestartedPods(c, restartingContainers)
			gomega.Expect(len(names)).To(gomega.Equal(0), "Some pods in got restarted during kube-apiserver rollout: %s", strings.Join(names, ", "))
		})
	})

})

func GetRestartedPods(c *kubernetes.Clientset, restartingContainers map[operators.ContainerName]int) (names []string) {
	pods := operators.GetPodsWithFilter(c, []operators.PodFilter{operators.InCoreNamespaces, ignoreNamespaces})
	for _, pod := range pods {
		if pod.Status.Phase == corev1.PodSucceeded {
			continue
		}
		// This will just load the restartingContainers map with the current restart count
		if operators.HasExcessiveRestarts(pod, 1, restartingContainers) {
			key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
			names = append(names, key)
		}
	}
	return names
}

func setRESTConfigDefaults(config *rest.Config) {
	if config.GroupVersion == nil {
		config.GroupVersion = &schema.GroupVersion{Group: "", Version: "v1"}
	}

	if config.NegotiatedSerializer == nil {
		config.NegotiatedSerializer = scheme.Codecs
	}
}

func forceApiRollout(oc *exutil.CLI) {
	redeploymentReason := fmt.Sprintf(`{"spec":{"forceRedeploymentReason":"resiliency-test-%v"}}`, uuid.NewUUID())

	_, err := oc.AdminOperatorClient().OperatorV1().KubeAPIServers().Patch(context.Background(), "cluster", types.MergePatchType,
		[]byte(redeploymentReason), metav1.PatchOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
}

func isApiReady(clusterConfig *rest.Config, httpClient *http.Client) (ready bool, reason string, err error) {
	resp, err := httpClient.Get(clusterConfig.Host + "/readyz")
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}

	if err != nil {
		return false, "client failed to make the request", err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		body, err := ioutil.ReadAll(resp.Body)
		if err == nil {
			return false, fmt.Sprintf("got status code %v from the server: %v", resp.Status, body), nil
		}

		return false, fmt.Sprintf("got status code %v from the server", resp.Status), err
	}

	return true, "kube-apiserver is ready", nil
}

func ignoreNamespaces(pod *corev1.Pod) bool {
	return !(strings.HasPrefix(pod.Namespace, "openshift-kube-apiserver") ||
		strings.HasPrefix(pod.Namespace, "openshift-kube-controller-manager")) // remove this once https://bugzilla.redhat.com/show_bug.cgi?id=2001330 is fixed
}
