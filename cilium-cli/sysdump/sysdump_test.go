// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package sysdump

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/blang/semver/v4"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/cilium/cilium/api/v1/models"
	"github.com/cilium/cilium/cilium-cli/defaults"
	"github.com/cilium/cilium/cilium-cli/k8s"
	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	ciliumv2alpha1 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2alpha1"
	"github.com/cilium/cilium/pkg/safeio"
)

type nopHooks struct{}

func (h *nopHooks) AddSysdumpFlags(*pflag.FlagSet)   {}
func (h *nopHooks) AddSysdumpTasks(*Collector) error { return nil }

func TestSysdumpCollector(t *testing.T) {
	client := fakeClient{
		nodeList: &corev1.NodeList{
			Items: []corev1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}},
			},
		},
	}
	options := Options{
		OutputFileName: "my-sysdump-<ts>",
		Writer:         io.Discard,
	}
	startTime := time.Unix(946713600, 0)
	timestamp := startTime.Format(timeFormat)
	collector, err := NewCollector(&client, options, &nopHooks{}, startTime)
	assert.NoError(t, err)
	assert.Equal(t, "my-sysdump-"+timestamp, path.Base(collector.sysdumpDir))
	tempFile := collector.AbsoluteTempPath("my-file-<ts>")
	assert.Equal(t, path.Join(collector.sysdumpDir, "my-file-"+timestamp), tempFile)
	_, err = os.Stat(path.Join(collector.sysdumpDir, sysdumpLogFile))
	assert.NoError(t, err)
}

func TestNodeList(t *testing.T) {
	options := Options{
		Writer: io.Discard,
	}
	client := fakeClient{
		nodeList: &corev1.NodeList{
			Items: []corev1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "node-c"}},
			},
		},
	}
	collector, err := NewCollector(&client, options, &nopHooks{}, time.Now())
	assert.NoError(t, err)
	assert.Equal(t, []string{"node-a", "node-b", "node-c"}, collector.NodeList)

	options = Options{
		Writer:   io.Discard,
		NodeList: "node-a,node-c",
	}
	collector, err = NewCollector(&client, options, &nopHooks{}, time.Now())
	assert.NoError(t, err)
	assert.Equal(t, []string{"node-a", "node-c"}, collector.NodeList)
}

type extendingHooks struct{}

func (h *extendingHooks) AddSysdumpFlags(*pflag.FlagSet) {}
func (h *extendingHooks) AddSysdumpTasks(c *Collector) error {
	c.AddTasks([]Task{
		{
			Description: "extended",
		},
	})
	return nil
}

func TestAddTasks(t *testing.T) {
	options := Options{
		Writer: io.Discard,
	}
	client := fakeClient{
		nodeList: &corev1.NodeList{
			Items: []corev1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}},
			},
		},
	}
	collector, err := NewCollector(&client, options, &nopHooks{}, time.Now())
	assert.NoError(t, err)
	assert.Empty(t, collector.additionalTasks)
	collector.AddTasks([]Task{{}, {}, {}})
	assert.Len(t, collector.additionalTasks, 3)
	collector.AddTasks([]Task{{}, {}, {}})
	assert.Len(t, collector.additionalTasks, 6)

	collector, err = NewCollector(&client, options, &extendingHooks{}, time.Now())
	assert.NoError(t, err)
	assert.Len(t, collector.additionalTasks, 1)
	assert.Equal(t, "extended", collector.additionalTasks[0].Description)
	collector.AddTasks([]Task{{}, {}})
	assert.Len(t, collector.additionalTasks, 3)
	assert.Equal(t, "extended", collector.additionalTasks[0].Description)
	collector.AddTasks([]Task{{}, {}, {}})
	assert.Len(t, collector.additionalTasks, 6)
	assert.Equal(t, "extended", collector.additionalTasks[0].Description)

}

func TestExtractGopsPID(t *testing.T) {
	var pid string
	var err error

	normalOutput := `
25863 0     gops          unknown Go version /usr/bin/gops
25852 25847 cilium        unknown Go version /usr/bin/cilium
10    1     cilium-agent* unknown Go version /usr/bin/cilium-agent
1     0     custom        go1.16.3           /usr/local/bin/custom
	`
	pid, err = extractGopsPID(normalOutput)
	assert.NoError(t, err)
	assert.Equal(t, "10", pid)

	missingAgent := `
25863 0     gops          unknown Go version /usr/bin/gops
25852 25847 cilium        unknown Go version /usr/bin/cilium
10    1     cilium-agent unknown Go version /usr/bin/cilium-agent
1     0     custom        go1.16.3           /usr/local/bin/custom
	`
	pid, err = extractGopsPID(missingAgent)
	assert.Error(t, err)
	assert.Empty(t, pid)

	multipleAgents := `
25863 0     gops*          unknown Go version /usr/bin/gops
25852 25847 cilium*        unknown Go version /usr/bin/cilium
10    1     cilium-agent unknown Go version /usr/bin/cilium-agent
1     0     custom        go1.16.3           /usr/local/bin/custom
	`
	pid, err = extractGopsPID(multipleAgents)
	assert.NoError(t, err)
	assert.Equal(t, "25863", pid)

	noOutput := ``
	_, err = extractGopsPID(noOutput)
	assert.Error(t, err)

}

func TestExtractGopsProfileData(t *testing.T) {
	gopsOutput := `
	Profiling CPU now, will take 30 secs...
	Profile dump saved to: /tmp/cpu_profile3302111893
	`
	wantFilepath := "/tmp/cpu_profile3302111893"

	gotFilepath, err := extractGopsProfileData(gopsOutput)
	assert.NoError(t, err)
	assert.Equal(t, wantFilepath, gotFilepath)

}

func TestKVStoreTask(t *testing.T) {
	assert := assert.New(t)
	client := &fakeClient{
		nodeList: &corev1.NodeList{
			Items: []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}},
		},
		execs: make(map[execRequest]execResult),
	}
	addKVStoreGet := func(c *fakeClient, ciliumPaths ...string) {
		for _, path := range ciliumPaths {
			c.expectExec("ns0", "pod0", defaults.AgentContainerName,
				[]string{"cilium", "kvstore", "get", "cilium/" + path, "--recursive", "-o", "json"},
				[]byte("{}"), nil, nil)
		}
	}
	addKVStoreGet(client, "state/identities", "state/ip", "state/nodes", "state/cnpstatuses", ".heartbeat", "state/services")
	options := Options{
		OutputFileName: "my-sysdump-<ts>",
		Writer:         io.Discard,
	}
	collector, err := NewCollector(client, options, &nopHooks{}, time.Now())
	assert.NoError(err)
	collector.submitKVStoreTasks(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod0",
			Namespace: "ns0",
		},
	})
	fd, err := os.Open(path.Join(collector.sysdumpDir, "kvstore-heartbeat.json"))
	assert.NoError(err)
	data, err := safeio.ReadAllLimit(fd, safeio.KB)
	assert.NoError(err)
	assert.Equal([]byte("{}"), data)
}

func TestListCiliumEndpointSlices(t *testing.T) {
	assert := assert.New(t)
	client := &fakeClient{}

	endpointSlices, err := client.ListCiliumEndpointSlices(context.Background(), metav1.ListOptions{})
	assert.NoError(err)
	assert.Len(endpointSlices.Items, 1)
}

func TestFilterPods(t *testing.T) {
	crashingPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "crashingPod",
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "CrashLoopBackOff",
						},
					},
				},
			},
		},
	}
	crashingInitContainerPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "crashingInitContainerPod",
		},
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{
				{
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "CrashLoopBackOff",
						},
					},
				},
			},
		},
	}
	restartedInitContainerPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "restartedInitContainerPod",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			InitContainerStatuses: []corev1.ContainerStatus{
				{
					RestartCount: 1,
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							Reason: "Error",
						},
					},
				},
			},
		},
	}
	runningReadyPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "runningReadyPod",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
	notRunningPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nonRunningPod",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
		},
	}
	notReadyPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "notReadyPod",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionFalse,
				},
			},
		},
	}

	podList := &corev1.PodList{
		Items: []corev1.Pod{crashingPod, crashingInitContainerPod, restartedInitContainerPod,
			runningReadyPod, notRunningPod, notReadyPod},
	}
	result := filterCrashedPods(podList, 0)
	assert.Len(t, result, 2)
	assert.Equal(t, crashingPod.Name, result[0].Name)
	assert.Equal(t, crashingInitContainerPod.Name, result[1].Name)

	result = filterRunningNotReadyPods(podList, 0)
	assert.Len(t, result, 1)
	assert.Equal(t, notReadyPod.Name, result[0].Name)

	result = filterRestartedContainersPods(podList, 0)
	assert.Len(t, result, 1)
	assert.Equal(t, restartedInitContainerPod.Name, result[0].Name)
}

func TestFilterPodsLimit(t *testing.T) {
	examplePod := corev1.Pod{}
	podList := &corev1.PodList{
		Items: []corev1.Pod{examplePod, examplePod, examplePod, examplePod, examplePod},
	}
	filterFunc := func(po *corev1.Pod) bool {
		return true
	}
	testCases := []struct {
		limit   int
		wantLen int
	}{
		{
			limit:   0,
			wantLen: 5,
		},
		{
			limit:   3,
			wantLen: 3,
		},
		{
			limit:   100,
			wantLen: 5,
		},
	}
	for _, tc := range testCases {
		result := filterPods(podList, filterFunc, tc.limit)
		assert.Len(t, result, tc.wantLen)
	}
}

type execRequest struct {
	namespace string
	pod       string
	container string
	command   string
}

type execResult struct {
	stderr []byte
	stdout []byte
	err    error
}

type fakeClient struct {
	nodeList *corev1.NodeList
	execs    map[execRequest]execResult
}

func (c *fakeClient) ListCiliumBGPPeeringPolicies(_ context.Context, _ metav1.ListOptions) (*ciliumv2alpha1.CiliumBGPPeeringPolicyList, error) {
	panic("implement me")
}

func (c *fakeClient) ListCiliumBGPClusterConfigs(ctx context.Context, opts metav1.ListOptions) (*ciliumv2alpha1.CiliumBGPClusterConfigList, error) {
	panic("implement me")
}

func (c *fakeClient) ListCiliumBGPPeerConfigs(ctx context.Context, opts metav1.ListOptions) (*ciliumv2alpha1.CiliumBGPPeerConfigList, error) {
	panic("implement me")
}

func (c *fakeClient) ListCiliumBGPAdvertisements(ctx context.Context, opts metav1.ListOptions) (*ciliumv2alpha1.CiliumBGPAdvertisementList, error) {
	panic("implement me")
}

func (c *fakeClient) ListCiliumBGPNodeConfigs(ctx context.Context, opts metav1.ListOptions) (*ciliumv2alpha1.CiliumBGPNodeConfigList, error) {
	panic("implement me")
}

func (c *fakeClient) ListCiliumBGPNodeConfigOverrides(ctx context.Context, opts metav1.ListOptions) (*ciliumv2alpha1.CiliumBGPNodeConfigOverrideList, error) {
	panic("implement me")
}

func (c *fakeClient) ListCiliumNodeConfigs(_ context.Context, _ string, _ metav1.ListOptions) (*ciliumv2alpha1.CiliumNodeConfigList, error) {
	panic("implement me")
}

func (c *fakeClient) ListCiliumPodIPPools(_ context.Context, _ metav1.ListOptions) (*ciliumv2alpha1.CiliumPodIPPoolList, error) {
	panic("implement me")
}

func (c *fakeClient) ListCiliumClusterwideEnvoyConfigs(_ context.Context, _ metav1.ListOptions) (*ciliumv2.CiliumClusterwideEnvoyConfigList, error) {
	panic("implement me")
}

func (c *fakeClient) ListCiliumEnvoyConfigs(_ context.Context, _ string, _ metav1.ListOptions) (*ciliumv2.CiliumEnvoyConfigList, error) {
	panic("implement me")
}

func (c *fakeClient) ListIngresses(_ context.Context, _ metav1.ListOptions) (*networkingv1.IngressList, error) {
	panic("implement me")
}

func (c *fakeClient) CopyFromPod(_ context.Context, _, _, _, _, _ string, _ int) error {
	panic("implement me")
}

func (c *fakeClient) AutodetectFlavor(_ context.Context) k8s.Flavor {
	panic("implement me")
}

func (c *fakeClient) GetPod(_ context.Context, _, _ string, _ metav1.GetOptions) (*corev1.Pod, error) {
	panic("implement me")
}

func (c *fakeClient) GetRaw(_ context.Context, _ string) (string, error) {
	panic("implement me")
}

func (c *fakeClient) CreatePod(_ context.Context, _ string, _ *corev1.Pod, _ metav1.CreateOptions) (*corev1.Pod, error) {
	panic("implement me")
}

func (c *fakeClient) DeletePod(_ context.Context, _, _ string, _ metav1.DeleteOptions) error {
	panic("implement me")
}

func (c *fakeClient) expectExec(namespace, pod, container string, command []string, expectedStdout []byte, expectedStderr []byte, expectedErr error) {
	r := execRequest{namespace, pod, container, strings.Join(command, " ")}
	c.execs[r] = execResult{
		stdout: expectedStdout,
		stderr: expectedStderr,
		err:    expectedErr,
	}
}

func (c *fakeClient) ExecInPod(ctx context.Context, namespace, pod, container string, command []string) (bytes.Buffer, error) {
	stdout, _, err := c.ExecInPodWithStderr(ctx, namespace, pod, container, command)
	return stdout, err
}

func (c *fakeClient) ExecInPodWithStderr(_ context.Context, namespace, pod, container string, command []string) (bytes.Buffer, bytes.Buffer, error) {
	r := execRequest{namespace, pod, container, strings.Join(command, " ")}
	out, ok := c.execs[r]
	if !ok {
		panic(fmt.Sprintf("unexpected exec: %v", r))
	}
	return *bytes.NewBuffer(out.stdout), *bytes.NewBuffer(out.stderr), out.err
}

func (c *fakeClient) ExecInPodWithWriters(_, _ context.Context, namespace, pod, container string, command []string, stdout, stderr io.Writer) error {
	r := execRequest{namespace, pod, container, strings.Join(command, " ")}
	out, ok := c.execs[r]
	if !ok {
		panic(fmt.Sprintf("unexpected exec: %v", r))
	}

	fmt.Println("out: ", string(out.stdout))
	fmt.Println("err: ", string(out.stderr))

	stdout.Write(out.stdout)
	stderr.Write(out.stderr)
	return out.err
}

func (c *fakeClient) GetCiliumVersion(_ context.Context, _ *corev1.Pod) (*semver.Version, error) {
	panic("implement me")
}

func (c *fakeClient) GetConfigMap(_ context.Context, _, _ string, _ metav1.GetOptions) (*corev1.ConfigMap, error) {
	return &corev1.ConfigMap{}, nil
}

func (c *fakeClient) GetDaemonSet(_ context.Context, _, _ string, _ metav1.GetOptions) (*appsv1.DaemonSet, error) {
	return nil, nil
}

func (c *fakeClient) GetStatefulSet(_ context.Context, _, _ string, _ metav1.GetOptions) (*appsv1.StatefulSet, error) {
	return nil, nil
}

func (c *fakeClient) GetCronJob(_ context.Context, _, _ string, _ metav1.GetOptions) (*batchv1.CronJob, error) {
	return nil, nil
}

func (c *fakeClient) GetDeployment(_ context.Context, _, _ string, _ metav1.GetOptions) (*appsv1.Deployment, error) {
	return nil, nil
}

func (c *fakeClient) GetLogs(_ context.Context, _, _, _ string, _ corev1.PodLogOptions, _ io.Writer) error {
	panic("implement me")
}

func (c *fakeClient) GetPodsTable(_ context.Context) (*metav1.Table, error) {
	panic("implement me")
}

func (c *fakeClient) ProxyGet(_ context.Context, namespace, name, url string) (string, error) {
	return fmt.Sprintf("Get from %s/%s/%s", namespace, name, url), nil
}

func (c *fakeClient) ProxyTCP(_ context.Context, _, _ string, _ uint16, _ func(io.ReadWriteCloser) error) error {
	panic("implement me")
}

func (c *fakeClient) GetSecret(_ context.Context, _, _ string, _ metav1.GetOptions) (*corev1.Secret, error) {
	panic("implement me")
}

func (c *fakeClient) GetVersion(_ context.Context) (string, error) {
	panic("implement me")
}

func (c *fakeClient) GetHelmMetadata(_ context.Context, _ string, _ string) (string, error) {
	panic("implement me")
}

func (c *fakeClient) GetHelmValues(_ context.Context, _ string, _ string) (string, error) {
	panic("implement me")
}

func (c *fakeClient) ListCiliumCIDRGroups(_ context.Context, _ metav1.ListOptions) (*ciliumv2alpha1.CiliumCIDRGroupList, error) {
	panic("implement me")
}

func (c *fakeClient) ListCiliumClusterwideNetworkPolicies(_ context.Context, _ metav1.ListOptions) (*ciliumv2.CiliumClusterwideNetworkPolicyList, error) {
	panic("implement me")
}

func (c *fakeClient) ListCiliumIdentities(_ context.Context) (*ciliumv2.CiliumIdentityList, error) {
	panic("implement me")
}

func (c *fakeClient) ListCiliumEgressGatewayPolicies(_ context.Context, _ metav1.ListOptions) (*ciliumv2.CiliumEgressGatewayPolicyList, error) {
	panic("implement me")
}

func (c *fakeClient) ListCiliumEndpoints(_ context.Context, _ string, _ metav1.ListOptions) (*ciliumv2.CiliumEndpointList, error) {
	panic("implement me")
}

func (c *fakeClient) ListCiliumEndpointSlices(_ context.Context, _ metav1.ListOptions) (*ciliumv2alpha1.CiliumEndpointSliceList, error) {
	ciliumEndpointSliceList := ciliumv2alpha1.CiliumEndpointSliceList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "List",
			APIVersion: "v1",
		},
		ListMeta: metav1.ListMeta{},
		Items: []ciliumv2alpha1.CiliumEndpointSlice{{
			TypeMeta: metav1.TypeMeta{
				Kind:       "CiliumEndpointSlice",
				APIVersion: "v2alpha1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: "testEndpointSlice1",
			},
			Endpoints: []ciliumv2alpha1.CoreCiliumEndpoint{{
				Name:       "EndpointSlice1",
				IdentityID: 1,
				Networking: &ciliumv2.EndpointNetworking{
					Addressing: ciliumv2.AddressPairList{{
						IPV4: "10.0.0.1",
					},
						{
							IPV4: "10.0.0.2",
						},
					},
				},
				Encryption: ciliumv2.EncryptionSpec{},
				NamedPorts: models.NamedPorts{},
			},
			},
		},
		},
	}
	return &ciliumEndpointSliceList, nil
}

func (c *fakeClient) ListCiliumLocalRedirectPolicies(_ context.Context, _ string, _ metav1.ListOptions) (*ciliumv2.CiliumLocalRedirectPolicyList, error) {
	panic("implement me")
}

func (c *fakeClient) ListCiliumNetworkPolicies(_ context.Context, _ string, _ metav1.ListOptions) (*ciliumv2.CiliumNetworkPolicyList, error) {
	panic("implement me")
}

func (c *fakeClient) ListCiliumNodes(_ context.Context) (*ciliumv2.CiliumNodeList, error) {
	panic("implement me")
}

func (c *fakeClient) ListDaemonSet(_ context.Context, _ string, _ metav1.ListOptions) (*appsv1.DaemonSetList, error) {
	panic("implement me")
}

func (c *fakeClient) ListEvents(_ context.Context, _ metav1.ListOptions) (*corev1.EventList, error) {
	panic("implement me")
}

func (c *fakeClient) ListNamespaces(_ context.Context, _ metav1.ListOptions) (*corev1.NamespaceList, error) {
	panic("implement me")
}

func (c *fakeClient) ListEndpoints(_ context.Context, _ metav1.ListOptions) (*corev1.EndpointsList, error) {
	panic("implement me")
}

func (c *fakeClient) ListEndpointSlices(_ context.Context, _ metav1.ListOptions) (*discoveryv1.EndpointSliceList, error) {
	panic("implement me")
}

func (c *fakeClient) ListNetworkPolicies(_ context.Context, _ metav1.ListOptions) (*networkingv1.NetworkPolicyList, error) {
	panic("implement me")
}

func (c *fakeClient) ListNodes(_ context.Context, _ metav1.ListOptions) (*corev1.NodeList, error) {
	return c.nodeList, nil
}

func (c *fakeClient) ListPods(_ context.Context, _ string, _ metav1.ListOptions) (*corev1.PodList, error) {
	return &corev1.PodList{}, nil
}

func (c *fakeClient) ListServices(_ context.Context, _ string, _ metav1.ListOptions) (*corev1.ServiceList, error) {
	panic("implement me")
}

func (c *fakeClient) ListUnstructured(_ context.Context, _ schema.GroupVersionResource, _ *string, _ metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	panic("implement me")
}

func (c *fakeClient) ListIngressClasses(_ context.Context, _ metav1.ListOptions) (*networkingv1.IngressClassList, error) {
	panic("implement me")
}

func (c *fakeClient) CreateEphemeralContainer(_ context.Context, _ *corev1.Pod, _ *corev1.EphemeralContainer) (*corev1.Pod, error) {
	panic("implement me")
}

func (c *fakeClient) GetNamespace(_ context.Context, ns string, _ metav1.GetOptions) (*corev1.Namespace, error) {
	if ns == "kube-system" {
		return &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: ns,
			},
		}, nil
	}
	return nil, &errors.StatusError{
		ErrStatus: metav1.Status{
			Code: http.StatusNotFound,
		},
	}
}

func Test_removeTopDirectory(t *testing.T) {
	result, err := removeTopDirectory("/")
	assert.NoError(t, err)
	assert.Empty(t, result)

	result, err = removeTopDirectory("a/b/c")
	assert.NoError(t, err)
	assert.Equal(t, "b/c", result)

	_, err = removeTopDirectory("")
	assert.Error(t, err)
}
