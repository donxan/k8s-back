package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/pflag"
	yaml "gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

// version 变量将在编译时由 Makefile 注入，用于显示程序版本
var version string = "unknown" // 默认值，如果未通过 ldflags 注入则显示此值

// ResourceKindMap 资源类型到 Kind 的映射
var ResourceKindMap = map[string]string{
	"configmaps":               "ConfigMap",
	"deployments":              "Deployment",
	"secrets":                  "Secret",
	"services":                 "Service",
	"persistentvolumeclaims":   "PersistentVolumeClaim",
	"statefulsets":             "StatefulSet",
	"horizontalpodautoscalers": "HorizontalPodAutoscaler",
	"cronjobs":                 "CronJob",
	"jobs":                     "Job",
	"persistentvolumes":        "PersistentVolume",
	"serviceaccounts":          "ServiceAccount",
}

// GroupVersionResourceMap 资源类型到 GroupVersionResource 的映射
var GroupVersionResourceMap = map[string]schema.GroupVersionResource{
	"configmaps":               {Group: "", Version: "v1", Resource: "configmaps"},
	"deployments":              {Group: "apps", Version: "v1", Resource: "deployments"},
	"secrets":                  {Group: "", Version: "v1", Resource: "secrets"},
	"services":                 {Group: "", Version: "v1", Resource: "services"},
	"persistentvolumeclaims":   {Group: "", Version: "v1", Resource: "persistentvolumeclaims"},
	"statefulsets":             {Group: "apps", Version: "v1", Resource: "statefulsets"},
	"horizontalpodautoscalers": {Group: "autoscaling", Version: "v2", Resource: "horizontalpodautoscalers"},
	"cronjobs":                 {Group: "batch", Version: "v1", Resource: "cronjobs"},
	"jobs":                     {Group: "batch", Version: "v1", Resource: "jobs"},
	"persistentvolumes":        {Group: "", Version: "v1", Resource: "persistentvolumes"},
	"serviceaccounts":          {Group: "", Version: "v1", Resource: "serviceaccounts"},
}

// CleanResource 清理资源中无用字段
func CleanResource(resource map[string]interface{}) map[string]interface{} {
	if resource == nil {
		return nil
	}

	cleanedResource := make(map[string]interface{})
	for k, v := range resource {
		cleanedResource[k] = v
	}

	metadata, ok := cleanedResource["metadata"].(map[string]interface{})
	if ok {
		for _, field := range []string{"creationTimestamp", "resourceVersion", "selfLink", "uid", "managedFields", "generation"} {
			delete(metadata, field)
		}

		if annotations, ok := metadata["annotations"].(map[string]interface{}); ok {
			delete(annotations, "kubectl.kubernetes.io/last-applied-configuration")
			if len(annotations) == 0 {
				delete(metadata, "annotations")
			} else {
				metadata["annotations"] = annotations
			}
		}

		for _, field := range []string{"annotations", "labels", "finalizers"} {
			if val, exists := metadata[field]; exists {
				if m, isMap := val.(map[string]interface{}); isMap && len(m) == 0 {
					delete(metadata, field)
				}
			}
		}
	}

	delete(cleanedResource, "status")

	kind, _ := cleanedResource["kind"].(string)
	if kind == "Deployment" {
		if spec, ok := cleanedResource["spec"].(map[string]interface{}); ok {
			if selector, ok := spec["selector"].(map[string]interface{}); ok {
				if matchLabels, ok := selector["matchLabels"].(map[string]interface{}); ok {
					if template, ok := spec["template"].(map[string]interface{}); ok {
						if tmplMetadata, ok := template["metadata"].(map[string]interface{}); ok {
							if tmplLabels, ok := tmplMetadata["labels"].(map[string]interface{}); ok {
								if mapsEqual(matchLabels, tmplLabels) {
									delete(selector, "matchLabels")
								}
							}
						}
					}
				}
			}
		}
	} else if kind == "Service" {
		if spec, ok := cleanedResource["spec"].(map[string]interface{}); ok {
			for _, field := range []string{"clusterIP", "clusterIPs", "internalTrafficPolicy", "externalTrafficPolicy", "ipFamilies", "ipFamilyPolicy", "sessionAffinityConfig"} {
				delete(spec, field)
			}
			if serviceType, ok := spec["type"].(string); ok && serviceType != "NodePort" {
				if ports, ok := spec["ports"].([]interface{}); ok {
					for _, p := range ports {
						if portMap, isMap := p.(map[string]interface{}); isMap {
							delete(portMap, "nodePort")
						}
					}
				}
			}
		}
	} else if kind == "PersistentVolume" {
		if spec, ok := cleanedResource["spec"].(map[string]interface{}); ok {
			delete(spec, "claimRef")
		}
	}

	return cleanedResource
}

// mapsEqual 比较两个 map[string]interface{} 是否相等
func mapsEqual(m1, m2 map[string]interface{}) bool {
	if len(m1) != len(m2) {
		return false
	}
	for k, v1 := range m1 {
		v2, ok := m2[k]
		if !ok || fmt.Sprintf("%v", v1) != fmt.Sprintf("%v", v2) {
			return false
		}
	}
	return true
}

// ShouldBackupSecret 判断 Secret 是否需要备份
func ShouldBackupSecret(secretObj map[string]interface{}) bool {
	metadata, ok := secretObj["metadata"].(map[string]interface{})
	if !ok {
		return false
	}
	name, _ := metadata["name"].(string)
	secretType, _ := secretObj["type"].(string)

	if strings.Contains(name, "default-token") || strings.HasPrefix(name, "sh.helm.release.v1.") {
		return false
	}
	if secretType == string(corev1.SecretTypeDockerConfigJson) ||
		secretType == string(corev1.SecretTypeServiceAccountToken) ||
		secretType == "helm.sh/release.v1" {
		return false
	}
	return true
}

func main() {
	var kubeconfig string
	var namespace string
	var resourceTypesStr string
	var outputDir string
	var showVersion bool // 新增：版本标志

	pflag.StringVar(&kubeconfig, "kubeconfig", "", "(可选) kubeconfig 文件路径。如果未指定，将使用默认查找顺序 (KUBECONFIG 环境变量或 ~/.kube/config)。")
	pflag.StringVarP(&namespace, "namespace", "n", "all", "指定要备份的命名空间 (例如: 'my-namespace')。使用 'all' (默认) 备份所有命名空间。")
	pflag.StringVarP(&resourceTypesStr, "type", "t", "", "指定一个或多个要备份的资源类型，用逗号分隔 (例如: 'deployments,secrets')。如果不指定，将备份所有支持的类型。")
	pflag.StringVarP(&outputDir, "output-dir", "o", ".", "指定备份文件的根目录。默认备份到当前目录。")
	// 新增：定义 --version 或 -v 标志
	pflag.BoolVarP(&showVersion, "version", "v", false, "显示程序版本信息。")
	pflag.Parse()

	// 如果指定了 --version 或 -v 标志，则打印版本并退出
	if showVersion {
		fmt.Printf("Kubernetes 备份工具版本: %s\n", version)
		return
	}

	// 以下是程序的正常备份逻辑
	// ...

	// 构建 Kubeconfig 配置
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		fmt.Printf("错误：无法加载 Kubernetes 配置: %v\n", err)
		fmt.Println("\n请检查以下几点以解决配置问题:")
		fmt.Println("  1. 确认您的 Kubernetes 集群正在运行且可访问。")
		fmt.Println("  2. 如果您在本地运行，请确保 kubeconfig 文件存在。")
		if kubeconfig != "" {
			fmt.Printf("     您已通过 --kubeconfig 参数指定了路径 '%s'，请检查该文件是否存在且内容有效。\n", kubeconfig)
		} else {
			fmt.Println("     程序将尝试在以下默认位置查找 kubeconfig 文件:")
			fmt.Println("       - 'KUBECONFIG' 环境变量指定的路径。")
			fmt.Println("       - 用户主目录下的 '.kube/config' 文件 (例如：Windows 系统上通常是 '%USERPROFILE%\\.kube\\config')。")
			fmt.Println("     如果这些位置没有有效的 kubeconfig，请手动通过 '--kubeconfig' 参数指定正确的路径。")
		}
		fmt.Println("  3. 您可以使用 'kubectl cluster-info' 命令来测试您的 Kubernetes 连接和配置。")
		os.Exit(1)
	}

	// 创建动态客户端
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		fmt.Printf("错误：创建 Kubernetes 动态客户端失败: %v\n", err)
		os.Exit(1)
	}

	// 构造最终的备份根目录路径
	finalBackupRoot := filepath.Join(outputDir, fmt.Sprintf("k8s-backup-%s", time.Now().Format("20060102150405")))
	err = os.MkdirAll(finalBackupRoot, os.ModePerm)
	if err != nil {
		fmt.Printf("错误：创建备份根目录失败 '%s': %v\n", finalBackupRoot, err)
		os.Exit(1)
	}

	// 确定要备份的资源类型
	var resourceTypesToBackup []string
	if resourceTypesStr != "" {
		resourceTypesToBackup = strings.Split(resourceTypesStr, ",")
	} else {
		for rType := range GroupVersionResourceMap {
			resourceTypesToBackup = append(resourceTypesToBackup, rType)
		}
	}

	totalBackedUpResources := 0

	// 处理每种资源
	for _, resTypePlural := range resourceTypesToBackup {
		kindName := ResourceKindMap[resTypePlural]
		gvr, ok := GroupVersionResourceMap[resTypePlural]
		if !ok {
			fmt.Printf("警告：不支持的资源类型 '%s'，跳过。\n", resTypePlural)
			continue
		}

		fmt.Printf("\n--- 正在处理 %ss ---\n", kindName)

		var resClient dynamic.ResourceInterface
		if namespace == "all" {
			resClient = dynamicClient.Resource(gvr)
		} else {
			resClient = dynamicClient.Resource(gvr).Namespace(namespace)
		}

		unstructuredList, err := resClient.List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			fmt.Printf("错误：获取 %s 资源失败: %v\n", resTypePlural, err)
			continue
		}

		resources := unstructuredList.Items
		if len(resources) == 0 {
			fmt.Printf("在 %s 中没有找到 %ss。\n", func() string {
				if namespace == "all" {
					return "所有命名空间"
				}
				return namespace
			}(), kindName)
			continue
		}

		if resTypePlural == "secrets" {
			initialSecretCount := len(resources)
			filteredUnstructuredSecrets := []unstructured.Unstructured{}
			for _, r := range resources {
				if ShouldBackupSecret(r.Object) {
					filteredUnstructuredSecrets = append(filteredUnstructuredSecrets, r)
				}
			}
			resources = filteredUnstructuredSecrets
			if len(resources) < initialSecretCount {
				fmt.Printf("过滤掉了 %d 个内部 Secret。\n", initialSecretCount-len(resources))
			}
		}

		if len(resources) == 0 {
			fmt.Printf("过滤后没有要备份的 %ss。\n", kindName)
			continue
		}

		backedUpCountForType := 0
		for _, resource := range resources {
			resourceMap := resource.Object

			cleaned := CleanResource(resourceMap)

			metadata, ok := cleaned["metadata"].(map[string]interface{})
			if !ok {
				fmt.Printf("警告：资源 %s 没有有效的元数据，跳过。\n", kindName)
				continue
			}
			name, ok := metadata["name"].(string)
			if !ok {
				fmt.Printf("警告：资源 %s 没有有效的名称，跳过。\n", kindName)
				continue
			}

			namespaceDir := "_cluster_"
			if ns, ok := metadata["namespace"].(string); ok && ns != "" {
				namespaceDir = ns
			}

			nsDir := filepath.Join(finalBackupRoot, namespaceDir)
			resourceTypeDir := filepath.Join(nsDir, resTypePlural)

			err = os.MkdirAll(resourceTypeDir, os.ModePerm)
			if err != nil {
				fmt.Printf("错误：创建目录 %s 失败: %v\n", resourceTypeDir, err)
				continue
			}

			outputData := map[string]interface{}{
				"apiVersion": cleaned["apiVersion"],
				"kind":       kindName,
				"metadata":   cleaned["metadata"],
			}

			if spec, ok := cleaned["spec"]; ok {
				outputData["spec"] = spec
			}
			if data, ok := cleaned["data"]; ok {
				outputData["data"] = data
			}
			if stringData, ok := cleaned["stringData"]; ok {
				outputData["stringData"] = stringData
			}
			if rules, ok := cleaned["rules"]; ok {
				outputData["rules"] = rules
			}

			yamlData, err := yaml.Marshal(outputData)
			if err != nil {
				fmt.Printf("警告：无法将资源 %s/%s 转换为 YAML: %v\n", namespaceDir, name, err)
				continue
			}

			filename := filepath.Join(resourceTypeDir, fmt.Sprintf("%s.yaml", name))
			err = os.WriteFile(filename, yamlData, 0644)
			if err != nil {
				fmt.Printf("警告：保存文件 %s 失败: %v\n", filename, err)
				continue
			}
			backedUpCountForType++
		}
		fmt.Printf("备份了 %d 个 %ss。\n", backedUpCountForType, kindName)
		totalBackedUpResources += backedUpCountForType
	}

	fmt.Printf("\n--- 备份完成 🎉 ---\n")
	fmt.Printf("备份目录: %s\n", finalBackupRoot)
	fmt.Printf("总计备份资源: %d 个\n", totalBackedUpResources)
	fmt.Println("\n要恢复资源，请导航到相应的资源类型和命名空间目录，然后应用 YAML 文件:")
	fmt.Println("  cd <您的自定义目录>/k8s-backup-<日期时间>/<namespace>/<resource_type>/")
	fmt.Println("  kubectl apply -f <resource_name>.yaml")
}
