package internal

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/transport/spdy"
)

var (
	kubeConfig = filepath.Join(os.Getenv("HOME"), ".kube", "config")
	clientMap  = make(map[string]*kubernetes.Clientset)
	mutex      sync.RWMutex
)

func getKubeRestConfig(kubectx string) (*rest.Config, error) {
	loadingRules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeConfig}
	overrides := &clientcmd.ConfigOverrides{
		CurrentContext: kubectx,
		ClusterInfo:    clientcmdapi.Cluster{},
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
}

// InitClients 初始化多个 context 的 Kubernetes client
func InitClients(contexts []string) error {

	for _, ctx := range contexts {
		restCfg, err := getKubeRestConfig(ctx)
		if err != nil {
			return fmt.Errorf("failed to get client config for context %s: %v", ctx, err)
		}

		clientset, err := kubernetes.NewForConfig(restCfg)
		if err != nil {
			return fmt.Errorf("failed to create client for context %s: %v", ctx, err)
		}

		mutex.Lock()
		clientMap[ctx] = clientset
		mutex.Unlock()
	}

	return nil
}

// GetClient 根据 context 名获取对应 client
func GetClient(context string) (*kubernetes.Clientset, error) {
	mutex.RLock()
	defer mutex.RUnlock()

	client, ok := clientMap[context]
	if !ok {
		return nil, fmt.Errorf("client for context %s not initialized", context)
	}
	return client, nil
}

func GetPodNameByLabel(kubectx string, namespace, labelSelector string) (string, error) {
	client, err := GetClient(kubectx)
	if err != nil {
		return "", fmt.Errorf("failed to get client: %w", err)
	}
	pods, err := client.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no pods found with label %s in namespace %s", labelSelector, namespace)
	}
	return pods.Items[0].Name, nil
}

func ExecToPod(client *kubernetes.Clientset, config *rest.Config, podName, namespace string, command []string) (string, string, error) {
	req := client.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Command: command,
			Stdin:   false,
			Stdout:  true,
			Stderr:  true,
			TTY:     false,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		return "", "", fmt.Errorf("failed to init SPDY executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.Stream(remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})

	return stdout.String(), stderr.String(), err
}

// ExecPodCommand 执行Pod中的命令的辅助函数
func ExecPodCommand(kubectx, namespace, podName string, command []string) (string, string, error) {
	client, err := GetClient(kubectx)
	if err != nil {
		return "", "", fmt.Errorf("failed to get client: %w", err)
	}

	config, err := getKubeRestConfig(kubectx)
	if err != nil {
		return "", "", fmt.Errorf("failed to get config: %w", err)
	}

	return ExecToPod(client, config, podName, namespace, command)
}

// PortForward 创建到 pod 的端口转发连接
func PortForward(kubectx, namespace, podName string, localPort, remotePort int) (func(), error) {
	client, err := GetClient(kubectx)
	if err != nil {
		return nil, fmt.Errorf("failed to get client: %w", err)
	}

	config, err := getKubeRestConfig(kubectx)
	if err != nil {
		return nil, fmt.Errorf("failed to get config: %w", err)
	}
	// 创建端口转发请求
	req := client.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(namespace).
		Name(podName).
		SubResource("portforward")

	transport, upgrader, err := spdy.RoundTripperFor(config)
	if err != nil {
		return nil, err
	}

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", req.URL())
	ports := []string{fmt.Sprintf("%d:%d", localPort, remotePort)}
	readyChannel := make(chan struct{}, 1)
	stopChannel := make(chan struct{}, 1)

	fw, err := portforward.NewOnAddresses(dialer,
		[]string{"localhost"},
		ports,
		stopChannel,
		readyChannel,
		os.Stdout,
		os.Stderr)
	if err != nil {
		return nil, err
	}

	// 在后台启动端口转发
	go func() {
		err := fw.ForwardPorts()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Port forwarding failed: %v\n", err)
		}
	}()

	// 等待就绪
	select {
	case <-readyChannel:
		return func() { close(stopChannel) }, nil
	case <-time.After(10 * time.Second):
		close(stopChannel)
		return nil, fmt.Errorf("timed out waiting for port forward to be ready")
	}
}
