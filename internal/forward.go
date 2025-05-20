package internal

import (
	"fmt"
	"os"
	"strings" // 用于构建聚合错误信息

	"github.com/emicklei/go-restful/v3/log"
)

// Forward 为指定站点和多个服务建立隧道。
// 它返回一个包含所有成功建立的隧道的停止函数的切片，以及一个错误对象。
// 如果部分服务失败，它仍然会返回成功服务的停止函数，错误对象会包含失败详情。
func Forward(site string, services ...string) ([]func(), error) {
	siteConfig, ok := Cfg.Sites[site]
	if !ok {
		return nil, fmt.Errorf("site %s not found in configuration", site)
	}
	if siteConfig.KubeContext == "" {
		return nil, fmt.Errorf("kubeContext not found for site %s", site)
	}

	var errs []error
	var successfulStopFuncs []func() // 存储成功启动的隧道的停止函数

	// 注意：InitClients 可能需要根据其原始设计来决定是否为每个站点或全局调用一次。
	// 这里假设它可以在这里被调用。
	err := InitClients([]string{siteConfig.KubeContext})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize clients for context %s: %w", siteConfig.KubeContext, err)
	}

	for _, service := range services {
		serviceConfig, ok := siteConfig.Services[service]
		if !ok {
			errs = append(errs, fmt.Errorf("service %s not found in site %s configuration", service, site))
			continue
		}

		localPort := serviceConfig.LocalPort
		remotePort := serviceConfig.DefaultPort
		address := serviceConfig.Endpoint // 这是 socat 在 Pod 内部连接的目标地址

		log.Printf("Attempting to start tunnel for service %s (local:%d -> pod_remote:%d -> target_in_pod:%s:%d)...\n",
			service, localPort, remotePort, address, remotePort)

		// 调用修改后的 startTunnel
		stopFunc, err := startTunnel(siteConfig, localPort, remotePort, address)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to start tunnel for service %s: %w", service, err))
		} else {
			successfulStopFuncs = append(successfulStopFuncs, stopFunc)
			log.Printf("🎉 Tunnel started successfully for service %s on local port %d. Remote socat forwards to %s:%d.\n",
				service, localPort, address, remotePort)
		}
	}

	if len(errs) > 0 {
		var errorMessages []string
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "❌ %v\n", e) // 仍然打印单个错误到 stderr
			errorMessages = append(errorMessages, e.Error())
		}
		// 返回成功启动的隧道的停止函数，并附带一个聚合的错误信息
		return successfulStopFuncs, fmt.Errorf("some services failed to start tunnels (%d/%d): %s",
			len(errs), len(services), strings.Join(errorMessages, "; "))
	}

	return successfulStopFuncs, nil // 所有服务均成功
}

// startTunnel 尝试为一个服务启动隧道。
// 它包括在 Pod 内设置和启动 socat，然后建立本地到 Pod 的端口转发。
// 返回一个用于停止端口转发的函数和可能的错误。
func startTunnel(siteConfig Site, localPort, remotePort int, targetAddressInPod string) (func(), error) {
	context := siteConfig.KubeContext
	labelSelector := siteConfig.Porxy
	namespace := siteConfig.Namespace
	podName, err := GetPodNameByLabel(siteConfig.KubeContext, namespace, labelSelector)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod name in namespace '%s' with selector '%s': %w", namespace, labelSelector, err)
	}
	log.Printf("🎯 Pod found: %s for context %s\n", podName, context)

	// 执行 Pod 内的 socat 初始化脚本
	// socat 将在 Pod 内监听 remotePort，并将流量转发到 targetAddressInPod:remotePort
	initScript := fmt.Sprintf(`
#!/bin/sh
set -e
echo "Ensuring socat and lsof are installed..."
if ! command -v socat >/dev/null 2>&1; then
  echo "socat not found, attempting to install..."
  if command -v apt-get >/dev/null 2>&1; then
    apt-get update && apt-get install -y socat
  elif command -v yum >/dev/null 2>&1; then
    yum install -y socat
  elif command -v apk >/dev/null 2>&1; then
    apk add --no-cache socat
  else
    echo "Error: Neither apt-get, yum, nor apk found. Cannot install socat." >&2
    exit 1
  fi
  echo "socat installed."
else
  echo "socat is already installed."
fi

if ! command -v lsof >/dev/null 2>&1; then
  echo "lsof not found, attempting to install..."
  if command -v apt-get >/dev/null 2>&1; then
    apt-get update && apt-get install -y lsof
  elif command -v yum >/dev/null 2>&1; then
    yum install -y lsof
  elif command -v apk >/dev/null 2>&1; then
    apk add --no-cache lsof
  else
    echo "Warning: Neither apt-get, yum, nor apk found. Cannot install lsof. socat check might be less reliable." >&2
  fi
  echo "lsof installed or package manager not found."
else
  echo "lsof is already installed."
fi

echo "Creating /tmp/run_socat_%d.sh..."
cat <<EOF > /tmp/run_socat_%d.sh
#!/bin/sh
# Check if socat is already listening on the port to avoid multiple instances
# Using -t for terse output, -i for IPv4/IPv6, -P for no port name resolution, -n for no hostname resolution
# and filtering for LISTEN state.
if command -v lsof >/dev/null 2>&1 && lsof -ti:%d -sTCP:LISTEN >/dev/null; then
  echo "socat (or another process) is already listening on port %d. Skipping socat startup."
else
  echo "Starting socat to listen on port %d and forward to %s:%d"
  nohup socat TCP-LISTEN:%d,fork,reuseaddr TCP:%s:%d >/tmp/socat_%d.log 2>&1 &
  echo "socat process launched in background."
fi
EOF
chmod +x /tmp/run_socat_%d.sh
echo "Init script finished."
`, remotePort, remotePort, remotePort, remotePort, remotePort, targetAddressInPod, remotePort, remotePort, targetAddressInPod, remotePort, remotePort, remotePort) // remotePort is used multiple times in script name and content

	// 1. 执行 init script
	log.Printf("Executing init script in pod %s...\n", podName)
	stdout, stderr, err := ExecPodCommand(context, namespace, podName, []string{"/bin/sh", "-c", initScript})
	if err != nil {
		return nil, fmt.Errorf("failed to exec init script in pod %s: %w\nstdout: %s\nstderr: %s", podName, err, stdout, stderr)
	}
	if stdout != "" {
		log.Printf("Init script stdout:\n%s\n", stdout)
	}
	if stderr != "" {
		// stderr from apt-get/yum can be noisy but not always fatal, treat as warning for now
		fmt.Fprintf(os.Stderr, "⚠️ Init script execution warnings/output on stderr for pod %s:\n%s\n", podName, stderr)
	}
	log.Printf("Init script executed.\n")

	// 2. 启动后台 socat 脚本 (run_socat_PORT.sh)
	runSocatScriptName := fmt.Sprintf("/tmp/run_socat_%d.sh", remotePort)
	log.Printf("Executing %s in pod %s to start socat...\n", runSocatScriptName, podName)
	stdout, stderr, err = ExecPodCommand(context, namespace, podName, []string{runSocatScriptName})
	if err != nil {
		return nil, fmt.Errorf("failed to start socat via %s in pod %s: %w\nstdout: %s\nstderr: %s", runSocatScriptName, podName, err, stdout, stderr)
	}
	if stdout != "" {
		log.Printf("Run socat script stdout:\n%s\n", stdout)
	}
	if stderr != "" {
		fmt.Fprintf(os.Stderr, "⚠️ Socat startup script (%s) warnings/output on stderr for pod %s:\n%s\n", runSocatScriptName, podName, stderr)
	}
	log.Printf("Run socat script executed in pod %s.\n", podName)

	// 3. 启动 port-forward (本地端口到 Pod 的 remotePort)
	log.Printf("Starting port-forward from localhost:%d to pod %s (namespace %s) remote port %d...\n", localPort, podName, namespace, remotePort)
	stopPortForwardFunc, err := PortForward(context, namespace, podName, localPort, remotePort)
	if err != nil {
		return nil, fmt.Errorf("failed to start port-forward from localhost:%d to pod %s remote port %d: %w", localPort, podName, remotePort, err)
	}
	// PortForward 成功，返回停止函数
	return stopPortForwardFunc, nil
}
