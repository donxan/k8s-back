package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// version 变量将在编译时由 Makefile 注入
var version string = "unknown" // 如果未通过 ldflags 注入，则显示此默认值

// ResourceInfo 定义了备份一个资源所需的所有信息
type ResourceInfo struct {
	Kind string
	GVR  schema.GroupVersionResource
}

// ResourceInfoMap 将资源类型映射到其详细信息
var ResourceInfoMap = map[string]ResourceInfo{
	"configmaps":               {Kind: "ConfigMap", GVR: schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}},
	"deployments":              {Kind: "Deployment", GVR: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}},
	"secrets":                  {Kind: "Secret", GVR: schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}},
	"services":                 {Kind: "Service", GVR: schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"}},
	"persistentvolumeclaims":   {Kind: "PersistentVolumeClaim", GVR: schema.GroupVersionResource{Group: "", Version: "v1", Resource: "persistentvolumeclaims"}},
	"statefulsets":             {Kind: "StatefulSet", GVR: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}},
	"horizontalpodautoscalers": {Kind: "HorizontalPodAutoscaler", GVR: schema.GroupVersionResource{Group: "autoscaling", Version: "v2", Resource: "horizontalpodautoscalers"}},
	"cronjobs":                 {Kind: "CronJob", GVR: schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"}},
	"jobs":                     {Kind: "Job", GVR: schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}},
	"persistentvolumes":        {Kind: "PersistentVolume", GVR: schema.GroupVersionResource{Group: "", Version: "v1", Resource: "persistentvolumes"}},
	"serviceaccounts":          {Kind: "ServiceAccount", GVR: schema.GroupVersionResource{Group: "", Version: "v1", Resource: "serviceaccounts"}},
}

// CleanResource 从资源清单中删除不必要的、由集群生成的字段
func CleanResource(resource map[string]interface{}) map[string]interface{} {
	if resource == nil {
		return nil
	}

	cleanedResource := make(map[string]interface{})
	for k, v := range resource {
		cleanedResource[k] = v
	}

	// 清理元数据
	if metadata, ok := cleanedResource["metadata"].(map[string]interface{}); ok {
		for _, field := range []string{"creationTimestamp", "resourceVersion", "selfLink", "uid", "managedFields", "generation"} {
			delete(metadata, field)
		}
		if annotations, ok := metadata["annotations"].(map[string]interface{}); ok {
			delete(annotations, "kubectl.kubernetes.io/last-applied-configuration")
			if len(annotations) == 0 {
				delete(metadata, "annotations")
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

	// 删除 status 字段，它总是在运行时生成
	delete(cleanedResource, "status")

	kind, _ := cleanedResource["kind"].(string)

	switch kind {
	case "Deployment":
		// 重要提示：不要删除 spec.selector。它是必需的且不可变的字段。
		break
	case "Service":
		if spec, ok := cleanedResource["spec"].(map[string]interface{}); ok {
			// 优化：仅删除纯粹由控制器管理的字段。
			// 保留用户可配置的字段，如 externalTrafficPolicy, internalTrafficPolicy,
			// 和 ipFamilyPolicy，这些对于恢复服务的原始行为至关重要。
			delete(spec, "clusterIP")
			delete(spec, "clusterIPs")

			// 仅当服务类型本身不使用 nodePort 时才删除它。
			if serviceType, ok := spec["type"].(string); ok && serviceType != "NodePort" && serviceType != "LoadBalancer" {
				if ports, ok := spec["ports"].([]interface{}); ok {
					for _, p := range ports {
						if portMap, isMap := p.(map[string]interface{}); isMap {
							delete(portMap, "nodePort")
						}
					}
				}
			}
		}
	case "PersistentVolume":
		if spec, ok := cleanedResource["spec"].(map[string]interface{}); ok {
			// claimRef 是一个动态绑定，不应包含在备份中。
			delete(spec, "claimRef")
		}
	}

	return cleanedResource
}

// ShouldBackupSecret 判断一个 Secret 是否应该被备份，过滤掉服务账户令牌。
func ShouldBackupSecret(secretObj map[string]interface{}) bool {
	metadata, ok := secretObj["metadata"].(map[string]interface{})
	if !ok {
		return false
	}
	name, _ := metadata["name"].(string)
	secretType, _ := secretObj["type"].(string)

	// 过滤掉自动生成的令牌和 Helm release secrets
	if strings.Contains(name, "-token-") || strings.HasPrefix(name, "sh.helm.release.v1.") {
		return false
	}

	switch corev1.SecretType(secretType) {
	case corev1.SecretTypeServiceAccountToken, "helm.sh/release.v1":
		return false
	}

	return true
}

// processStringMapValues 递归地清理 map 中的字符串值。
func processStringMapValues(m map[string]interface{}) {
	if m == nil {
		return
	}
	for k, v := range m {
		if s, isString := v.(string); isString {
			s = strings.ReplaceAll(s, "\r\n", "\n")  // 规范化换行符
			s = strings.ReplaceAll(s, "\\n", "\n")   // 解码换行符
			s = strings.ReplaceAll(s, "\\r", "\r")   // 解码回车符
			s = strings.ReplaceAll(s, "\u00A0", " ") // 替换不间断空格
			m[k] = s
		} else if subMap, isMap := v.(map[string]interface{}); isMap {
			processStringMapValues(subMap)
		}
	}
}

// canListResource 检查当前用户是否具有列出指定资源的权限。
func canListResource(clientset *kubernetes.Clientset, gvr schema.GroupVersionResource, namespace string) (bool, error) {
	sar := &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: namespace,
				Verb:      "list",
				Group:     gvr.Group,
				Resource:  gvr.Resource,
			},
		},
	}

	response, err := clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(context.TODO(), sar, metav1.CreateOptions{})
	if err != nil {
		return false, fmt.Errorf("检查权限失败: %w", err)
	}

	return response.Status.Allowed, nil
}

func main() {
	var kubeconfig, namespace, resourceTypesStr, outputDir string
	var showVersion bool

	pflag.StringVar(&kubeconfig, "kubeconfig", "", "(可选) kubeconfig 文件的路径")
	pflag.StringVarP(&namespace, "namespace", "n", "all", "要备份的命名空间 ('all' 表示所有命名空间)")
	pflag.StringVarP(&resourceTypesStr, "type", "t", "", "要备份的资源类型列表 (用逗号分隔)")
	pflag.StringVarP(&outputDir, "output-dir", "o", ".", "备份文件的根目录")
	pflag.BoolVarP(&showVersion, "version", "v", false, "显示版本信息")
	pflag.Parse()

	if showVersion {
		fmt.Printf("Kubernetes 备份工具版本: %s\n", version)
		return
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		fmt.Printf("错误: 无法加载 Kubernetes 配置: %v\n", err)
		os.Exit(1)
	}

	// 创建用于权限检查的标准 clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		fmt.Printf("错误: 无法创建 Kubernetes clientset: %v\n", err)
		os.Exit(1)
	}

	// 创建用于获取资源的动态 client
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		fmt.Printf("错误: 无法创建 Kubernetes 动态客户端: %v\n", err)
		os.Exit(1)
	}

	finalBackupRoot := filepath.Join(outputDir, fmt.Sprintf("k8s-backup-%s", time.Now().Format("20060102150405")))
	if err := os.MkdirAll(finalBackupRoot, os.ModePerm); err != nil {
		fmt.Printf("错误: 无法创建备份根目录 '%s': %v\n", finalBackupRoot, err)
		os.Exit(1)
	}

	var resourceTypesToBackup []string
	if resourceTypesStr != "" {
		resourceTypesToBackup = strings.Split(resourceTypesStr, ",")
	} else {
		for rType := range ResourceInfoMap {
			resourceTypesToBackup = append(resourceTypesToBackup, rType)
		}
	}

	totalBackedUpResources := 0

	for _, resTypePlural := range resourceTypesToBackup {
		resInfo, ok := ResourceInfoMap[resTypePlural]
		if !ok {
			fmt.Printf("警告: 不支持的资源类型 '%s'，已跳过。\n", resTypePlural)
			continue
		}

		fmt.Printf("\n--- 正在处理 %ss ---\n", resInfo.Kind)

		// === 权限检查 ===
		checkNS := namespace
		if namespace == "all" {
			checkNS = "" // 对于 'all'，在集群级别进行检查
		}
		allowed, err := canListResource(clientset, resInfo.GVR, checkNS)
		if err != nil {
			fmt.Printf("警告: 无法验证 '%s' 的权限，已跳过。错误: %v\n", resTypePlural, err)
			continue
		}
		if !allowed {
			nsMsg := fmt.Sprintf("命名空间 '%s'", namespace)
			if namespace == "all" {
				nsMsg = "所有命名空间"
			}
			fmt.Printf("警告: 权限不足，无法在 %s 中 'list' (列出) '%s' 类型的资源。已跳过。\n", nsMsg, resTypePlural)
			continue
		}
		// === 权限检查结束 ===

		var resClient dynamic.ResourceInterface
		if namespace == "all" {
			resClient = dynamicClient.Resource(resInfo.GVR)
		} else {
			resClient = dynamicClient.Resource(resInfo.GVR).Namespace(namespace)
		}

		unstructuredList, err := resClient.List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			fmt.Printf("错误: 列出 %s 资源失败: %v\n", resTypePlural, err)
			continue
		}

		resources := unstructuredList.Items
		if len(resources) == 0 {
			continue // 没有找到此类型的资源，继续下一个
		}

		if resTypePlural == "secrets" {
			var filteredResources []unstructured.Unstructured
			for _, r := range resources {
				if ShouldBackupSecret(r.Object) {
					filteredResources = append(filteredResources, r)
				}
			}
			resources = filteredResources
		}

		if len(resources) == 0 {
			fmt.Printf("没有找到可备份的用户管理的 %ss。\n", resInfo.Kind)
			continue
		}

		backedUpCountForType := 0
		for _, resource := range resources {
			resource.SetKind(resInfo.Kind)
			cleaned := CleanResource(resource.Object)

			metadata, _ := cleaned["metadata"].(map[string]interface{})
			name, _ := metadata["name"].(string)

			nsDirName := "_cluster_"
			if ns, found, _ := unstructured.NestedString(metadata, "namespace"); found && ns != "" {
				nsDirName = ns
			}

			resourceTypeDir := filepath.Join(finalBackupRoot, nsDirName, resTypePlural)
			if err := os.MkdirAll(resourceTypeDir, os.ModePerm); err != nil {
				fmt.Printf("错误: 无法创建目录 %s: %v\n", resourceTypeDir, err)
				continue
			}

			// 处理 ConfigMap 和 Secret 的 data/stringData 字段
			if data, found, _ := unstructured.NestedMap(cleaned, "data"); found {
				processStringMapValues(data)
			}
			if stringData, found, _ := unstructured.NestedMap(cleaned, "stringData"); found {
				processStringMapValues(stringData)
			}

			yamlData, err := yaml.Marshal(cleaned)
			if err != nil {
				fmt.Printf("警告: 无法将资源 %s/%s 转换为 YAML: %v\n", nsDirName, name, err)
				continue
			}

			filename := filepath.Join(resourceTypeDir, fmt.Sprintf("%s.yaml", name))
			if err := os.WriteFile(filename, yamlData, 0644); err != nil {
				fmt.Printf("警告: 无法保存文件 %s: %v\n", filename, err)
				continue
			}
			backedUpCountForType++
		}

		if backedUpCountForType > 0 {
			fmt.Printf("成功备份了 %d 个 %s。\n", backedUpCountForType, resInfo.Kind)
			totalBackedUpResources += backedUpCountForType
		}
	}

	fmt.Printf("\n--- 备份完成 🎉 ---\n")
	fmt.Printf("备份目录: %s\n", finalBackupRoot)
	fmt.Printf("总共备份的资源数量: %d\n", totalBackedUpResources)
	fmt.Println("\n要恢复资源，请应用其对应的 YAML 文件:")
	fmt.Println("  kubectl apply -f <备份目录>/<命名空间>/<资源类型>/<资源名称>.yaml")
}
