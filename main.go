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
	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var version string = "v2.1.4" // 默认版本号

// ResourceInfo 包含资源的完整定义
type ResourceInfo struct {
	Kind       string
	GVR        schema.GroupVersionResource
	CorePath   bool // 是否是core API组资源
	Namespaced bool
}

// 资源类型映射表
var resourceMap = map[string]ResourceInfo{
	"configmaps": {
		Kind: "ConfigMap", CorePath: true,
		GVR: schema.GroupVersionResource{
			Group:    "",
			Version:  "v1",
			Resource: "configmaps",
		},
		Namespaced: true,
	},
	"deployments": {
		Kind: "Deployment",
		GVR: schema.GroupVersionResource{
			Group:    "apps",
			Version:  "v1",
			Resource: "deployments",
		},
		Namespaced: true,
	},
	"secrets": {
		Kind: "Secret", CorePath: true,
		GVR: schema.GroupVersionResource{
			Group:    "",
			Version:  "v1",
			Resource: "secrets",
		},
		Namespaced: true,
	},
	"services": {
		Kind: "Service", CorePath: true,
		GVR: schema.GroupVersionResource{
			Group:    "",
			Version:  "v1",
			Resource: "services",
		},
		Namespaced: true,
	},
	"persistentvolumeclaims": {
		Kind: "PersistentVolumeClaim", CorePath: true,
		GVR: schema.GroupVersionResource{
			Group:    "",
			Version:  "v1",
			Resource: "persistentvolumeclaims",
		},
		Namespaced: true,
	},
	"statefulsets": {
		Kind: "StatefulSet",
		GVR: schema.GroupVersionResource{
			Group:    "apps",
			Version:  "v1",
			Resource: "statefulsets",
		},
		Namespaced: true,
	},
	"horizontalpodautoscalers": {
		Kind: "HorizontalPodAutoscaler",
		GVR: schema.GroupVersionResource{
			Group:    "autoscaling",
			Version:  "v2",
			Resource: "horizontalpodautoscalers",
		},
		Namespaced: true,
	},
	"cronjobs": {
		Kind: "CronJob",
		GVR: schema.GroupVersionResource{
			Group:    "batch",
			Version:  "v1",
			Resource: "cronjobs",
		},
		Namespaced: true,
	},
	"jobs": {
		Kind: "Job",
		GVR: schema.GroupVersionResource{
			Group:    "batch",
			Version:  "v1",
			Resource: "jobs",
		},
		Namespaced: true,
	},
	"persistentvolumes": {
		Kind: "PersistentVolume", CorePath: true,
		GVR: schema.GroupVersionResource{
			Group:    "",
			Version:  "v1",
			Resource: "persistentvolumes",
		},
		Namespaced: false,
	},
	"serviceaccounts": {
		Kind: "ServiceAccount", CorePath: true,
		GVR: schema.GroupVersionResource{
			Group:    "",
			Version:  "v1",
			Resource: "serviceaccounts",
		},
		Namespaced: true,
	},
	"ingresses": {
		Kind: "Ingress",
		GVR: schema.GroupVersionResource{
			Group:    "networking.k8s.io",
			Version:  "v1",
			Resource: "ingresses",
		},
		Namespaced: true,
	},
}

// CleanResource 清理资源中无用字段，保留必要配置
func CleanResource(resource map[string]interface{}) map[string]interface{} {
	if resource == nil {
		return nil
	}

	cleaned := make(map[string]interface{})
	for k, v := range resource {
		cleaned[k] = v
	}

	// 清理metadata字段
	metadata, ok := cleaned["metadata"].(map[string]interface{})
	if ok {
		// 移除自动生成的服务器字段
		for _, field := range []string{
			"creationTimestamp", "resourceVersion", "selfLink",
			"uid", "managedFields", "generation",
		} {
			delete(metadata, field)
		}

		// 清理annotations
		if annotations, ok := metadata["annotations"].(map[string]interface{}); ok {
			delete(annotations, "kubectl.kubernetes.io/last-applied-configuration")
			delete(annotations, "deployment.kubernetes.io/revision")

			// 清理空annotations
			if len(annotations) == 0 {
				delete(metadata, "annotations")
			}
		}

		// 清理空labels和finalizers
		for _, field := range []string{"labels", "finalizers"} {
			if val, exists := metadata[field]; exists {
				if m, isMap := val.(map[string]interface{}); isMap && len(m) == 0 {
					delete(metadata, field)
				}
			}
		}
	}

	// 移除状态信息
	delete(cleaned, "status")

	// 资源类型特定处理
	kind, _ := cleaned["kind"].(string)
	switch kind {
	case "Service":
		// 保留externalTrafficPolicy等流量策略配置
		if spec, ok := cleaned["spec"].(map[string]interface{}); ok {
			// 只移除集群特定字段，保留业务相关配置
			for _, field := range []string{"clusterIP", "clusterIPs", "ipFamilies", "ipFamilyPolicy"} {
				delete(spec, field)
			}
		}
	case "Deployment":
		if spec, ok := cleaned["spec"].(map[string]interface{}); ok {
			// 保留selector完整配置
			delete(spec, "progressDeadlineSeconds")
			delete(spec, "revisionHistoryLimit")
			// 保留但不自动设置副本数
			delete(spec, "replicas")
		}
	case "StatefulSet":
		if spec, ok := cleaned["spec"].(map[string]interface{}); ok {
			delete(spec, "revisionHistoryLimit")
			delete(spec, "replicas")
		}
	case "PersistentVolume":
		if spec, ok := cleaned["spec"].(map[string]interface{}); ok {
			// 保留但移除集群绑定信息
			delete(spec, "claimRef")
		}
	case "Pod":
		// Pod通常不需要备份，但这里保留逻辑
		delete(cleaned, "spec")
	}

	return cleaned
}

// ShouldBackupSecret 判断Secret是否需要备份
func ShouldBackupSecret(secretObj map[string]interface{}) bool {
	metadata, ok := secretObj["metadata"].(map[string]interface{})
	if !ok {
		return false
	}
	name, _ := metadata["name"].(string)
	secretType, _ := secretObj["type"].(string)

	// 跳过系统生成的Secret
	if strings.Contains(name, "default-token") ||
		strings.HasPrefix(name, "sh.helm.release.v1.") ||
		strings.Contains(name, "-token-") {
		return false
	}

	// 跳过特定类型的Secret
	excludedTypes := []string{
		string(corev1.SecretTypeDockerConfigJson),
		string(corev1.SecretTypeServiceAccountToken),
		string(corev1.SecretTypeBasicAuth),
		string(corev1.SecretTypeTLS),
		"helm.sh/release.v1",
	}
	for _, t := range excludedTypes {
		if secretType == t {
			return false
		}
	}

	return true
}

// processStringMapValues 标准化字符串值
func processStringMapValues(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	processed := make(map[string]interface{})
	for k, v := range m {
		switch val := v.(type) {
		case string:
			s := val
			s = strings.ReplaceAll(s, "\r\n", "\n") // Windows换行符转换
			s = strings.ReplaceAll(s, "\\n", "\n")  // 转义符解码
			s = strings.ReplaceAll(s, "\\t", "\t")
			s = strings.ReplaceAll(s, "\\r", "\r")
			s = strings.ReplaceAll(s, "\u00A0", " ") // 非中断空格处理
			processed[k] = s
		case map[string]interface{}:
			processed[k] = processStringMapValues(val)
		default:
			processed[k] = v
		}
	}
	return processed
}

// checkResourceAccess 检查当前用户是否有资源读取权限
func checkResourceAccess(
	clientset *kubernetes.Clientset,
	gvr schema.GroupVersionResource,
	namespace string,
	verb string,
) bool {
	ssar := &authv1.SelfSubjectAccessReview{
		Spec: authv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authv1.ResourceAttributes{
				Group:     gvr.Group,
				Version:   gvr.Version,
				Resource:  gvr.Resource,
				Verb:      verb,
				Namespace: namespace,
			},
		},
	}

	result, err := clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(
		context.TODO(), ssar, metav1.CreateOptions{})
	if err != nil {
		fmt.Printf("权限检查失败 [%s/%s]: %v\n", gvr.Resource, namespace, err)
		return false
	}

	return result.Status.Allowed
}

func main() {
	var kubeconfig string
	var namespace string
	var resourceTypesStr string
	var outputDir string
	var showVersion bool
	var skipNamespacesStr string
	var skipSecrets bool
	var skipClusterResources bool

	pflag.StringVar(&kubeconfig, "kubeconfig", "", "Kubeconfig文件路径")
	pflag.StringVarP(&namespace, "namespace", "n", "all", "备份命名空间 ('all' 备份所有命名空间)")
	pflag.StringVarP(&resourceTypesStr, "type", "t", "", "资源类型列表 (逗号分隔)")
	pflag.StringVarP(&outputDir, "output-dir", "o", ".", "备份目录")
	pflag.StringVarP(&skipNamespacesStr, "exclude-namespaces", "e", "kube-system", "排除的命名空间列表")
	pflag.BoolVarP(&skipSecrets, "skip-secrets", "s", false, "跳过所有Secret备份")
	pflag.BoolVarP(&skipClusterResources, "no-cluster-resources", "c", false, "跳过集群级资源")
	pflag.BoolVarP(&showVersion, "version", "v", false, "显示版本")
	pflag.Parse()

	// 打印版本信息
	if showVersion {
		fmt.Printf("Kubernetes资源备份工具 v%s\n", version)
		pflag.Usage()
		os.Exit(0)
	}

	// 配置加载
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		fmt.Printf("错误: 无法加载Kubernetes配置: %v\n", err)
		fmt.Println("排查建议:")
		fmt.Println("  1. 确认 kubeconfig 文件存在:`kubectl config view`")
		fmt.Println("  2. 检查集群连通性:`kubectl cluster-info`")
		os.Exit(1)
	}

	// 创建客户端
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		fmt.Printf("错误: 创建动态客户端失败: %v\n", err)
		os.Exit(1)
	}

	// 创建 Kubernetes 客户端（用于权限检查）
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		fmt.Printf("错误: 创建标准客户端失败: %v\n", err)
		os.Exit(1)
	}

	// 解析排除的命名空间
	skipNamespaces := strings.Split(skipNamespacesStr, ",")
	if len(skipNamespaces) == 0 {
		skipNamespaces = []string{"kube-system"}
	}

	// 准备备份目录
	timestamp := time.Now().Format("20060102-150405")
	backupRoot := filepath.Join(outputDir, fmt.Sprintf("k8s-backup-%s", timestamp))
	if err := os.MkdirAll(backupRoot, 0755); err != nil {
		fmt.Printf("错误: 创建备份目录失败: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("备份开始于: %s\n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Printf("备份目录: %s\n", backupRoot)
	fmt.Printf("排除命名空间: %v\n", skipNamespaces)
	if skipSecrets {
		fmt.Println("配置: 跳过所有Secret备份")
	}
	if skipClusterResources {
		fmt.Println("配置: 跳过集群级资源")
	}

	// 确定要备份的资源类型
	var resourceTypes []string
	if resourceTypesStr != "" {
		resourceTypes = strings.Split(resourceTypesStr, ",")
	} else {
		for resType := range resourceMap {
			resourceTypes = append(resourceTypes, resType)
		}
	}
	fmt.Printf("备份资源类型: %v\n", resourceTypes)

	// 获取命名空间
	var targetNamespaces []corev1.Namespace
	switch namespace {
	case "all":
		nsClient := dynamicClient.Resource(schema.GroupVersionResource{
			Group:    "",
			Version:  "v1",
			Resource: "namespaces",
		})

		nsList, err := nsClient.List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			fmt.Printf("警告: 获取命名空间失败: %v\n", err)
		} else {
			for _, ns := range nsList.Items {
				nsName := ns.GetName()
				skip := false
				for _, skipNS := range skipNamespaces {
					if nsName == skipNS {
						skip = true
						break
					}
				}
				if !skip {
					targetNamespaces = append(targetNamespaces, corev1.Namespace{
						ObjectMeta: metav1.ObjectMeta{
							Name: nsName,
						},
					})
				}
			}
		}
	default:
		targetNamespaces = []corev1.Namespace{{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}}
	}

	// 保存全局命名空间信息
	if !skipClusterResources {
		globalDir := filepath.Join(backupRoot, "_global")
		if err := os.MkdirAll(globalDir, 0755); err != nil {
			fmt.Printf("警告: 创建全局目录失败: %v\n", err)
		}
	}

	// 备份主循环
	totalResources := 0
	startTime := time.Now()

	for _, ns := range targetNamespaces {
		nsName := ns.Name
		fmt.Printf("\n[命名空间: %s]\n", nsName)
		nsDir := filepath.Join(backupRoot, nsName)
		if err := os.MkdirAll(nsDir, 0755); err != nil {
			fmt.Printf("警告: 创建命名空间目录失败: %v\n", err)
			continue
		}

		// 保存命名空间元数据
		nsYaml, err := yaml.Marshal(map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata": map[string]interface{}{
				"name": nsName,
			},
		})
		if err == nil {
			os.WriteFile(filepath.Join(nsDir, "00-namespace.yaml"), nsYaml, 0644)
		}

		// 备份命名空间的资源
		nsResources := 0
		for _, resType := range resourceTypes {
			resInfo, exists := resourceMap[resType]
			if !exists {
				fmt.Printf("  警告: 跳过不支持的类型: %s\n", resType)
				continue
			}

			// 检查权限
			if !checkResourceAccess(clientset, resInfo.GVR, nsName, "list") {
				fmt.Printf("  警告: 无权限读取 %s/%s，跳过\n", nsName, resInfo.Kind)
				continue
			}

			// 特殊处理Secret跳过
			if skipSecrets && resType == "secrets" {
				fmt.Printf("  配置跳过: %s\n", resInfo.Kind)
				continue
			}

			// 集群级资源放全局目录处理
			if !resInfo.Namespaced {
				if skipClusterResources {
					continue
				}
				fmt.Printf("  资源 %s 是集群级资源，将在全局目录处理\n", resInfo.Kind)
				continue
			}

			resClient := dynamicClient.Resource(resInfo.GVR).Namespace(nsName)
			resList, err := resClient.List(context.TODO(), metav1.ListOptions{})
			if err != nil {
				fmt.Printf("  错误: 获取 %s 失败: %v\n", resInfo.Kind, err)
				continue
			}

			resources := resList.Items
			if len(resources) == 0 {
				continue
			}

			fmt.Printf("  资源: %s (找到 %d 个)\n", resInfo.Kind, len(resources))

			// 创建资源类型目录
			resDir := filepath.Join(nsDir, resType)
			if err := os.MkdirAll(resDir, 0755); err != nil {
				fmt.Printf("    错误: 创建目录失败: %v\n", err)
				continue
			}

			// 特殊过滤逻辑
			if resType == "secrets" {
				filtered := []unstructured.Unstructured{}
				for _, r := range resources {
					if ShouldBackupSecret(r.Object) {
						filtered = append(filtered, r)
					}
				}
				fmt.Printf("    过滤后剩余 %d 个Secret\n", len(filtered))
				resources = filtered
			}

			backupCount := 0
			for _, resource := range resources {
				obj := resource.Object
				obj = CleanResource(obj)

				// 构建YAML结构
				resourceYAML := map[string]interface{}{
					"apiVersion": obj["apiVersion"],
					"kind":       obj["kind"],
					"metadata":   obj["metadata"],
				}

				// 添加核心字段
				if spec, hasSpec := obj["spec"]; hasSpec {
					resourceYAML["spec"] = spec
				}
				if data, hasData := obj["data"]; hasData {
					resourceYAML["data"] = data
				}
				if rules, hasRules := obj["rules"]; hasRules {
					resourceYAML["rules"] = rules
				}

				// 处理字符串转义问题
				if resType == "configmaps" {
					if data, ok := resourceYAML["data"].(map[string]interface{}); ok {
						resourceYAML["data"] = processStringMapValues(data)
					}
				}

				yamlData, err := yaml.Marshal(resourceYAML)
				if err != nil {
					fmt.Printf("    错误: 序列化失败: %v\n", err)
					continue
				}

				name := resource.GetName()
				filename := fmt.Sprintf("%s.yaml", name)
				fullPath := filepath.Join(resDir, filename)
				if err := os.WriteFile(fullPath, yamlData, 0644); err != nil {
					fmt.Printf("    错误: 写入文件失败: %v\n", err)
					continue
				}

				backupCount++
			}

			fmt.Printf("    ✓ 备份 %d 个 %s\n", backupCount, resInfo.Kind)
			nsResources += backupCount
			totalResources += backupCount
		}
	}

	// 备份集群范围资源（如果不跳过）
	if !skipClusterResources {
		fmt.Println("\n[集群范围资源]")
		globalDir := filepath.Join(backupRoot, "_global")

		// 创建集群级资源目录
		if err := os.MkdirAll(globalDir, 0755); err != nil {
			fmt.Printf("警告: 创建全局目录失败: %v\n", err)
		} else {
			for _, resType := range resourceTypes {
				resInfo, exists := resourceMap[resType]
				if !exists || resInfo.Namespaced {
					continue
				}

				// 检查权限
				if !checkResourceAccess(clientset, resInfo.GVR, "", "list") {
					fmt.Printf("  警告: 无权限读取 %s，跳过\n", resInfo.Kind)
					continue
				}

				resClient := dynamicClient.Resource(resInfo.GVR)
				resList, err := resClient.List(context.TODO(), metav1.ListOptions{})
				if err != nil {
					fmt.Printf("  错误: 获取 %s 失败: %v\n", resInfo.Kind, err)
					continue
				}

				resources := resList.Items
				if len(resources) == 0 {
					continue
				}

				fmt.Printf("  资源: %s (找到 %d 个)\n", resInfo.Kind, len(resources))

				resDir := filepath.Join(globalDir, resType)
				if err := os.MkdirAll(resDir, 0755); err != nil {
					fmt.Printf("    错误: 创建目录失败: %v\n", err)
					continue
				}

				backupCount := 0
				for _, resource := range resources {
					obj := resource.Object
					obj = CleanResource(obj)

					resourceYAML := map[string]interface{}{
						"apiVersion": obj["apiVersion"],
						"kind":       obj["kind"],
						"metadata":   obj["metadata"],
					}

					if spec, hasSpec := obj["spec"]; hasSpec {
						resourceYAML["spec"] = spec
					}

					yamlData, err := yaml.Marshal(resourceYAML)
					if err != nil {
						fmt.Printf("    错误: 序列化失败: %v\n", err)
						continue
					}

					name := resource.GetName()
					filename := fmt.Sprintf("%s.yaml", name)
					fullPath := filepath.Join(resDir, filename)
					if err := os.WriteFile(fullPath, yamlData, 0644); err != nil {
						fmt.Printf("    错误: 写入文件失败: %v\n", err)
						continue
					}

					backupCount++
				}

				fmt.Printf("    ✓ 备份 %d 个 %s\n", backupCount, resInfo.Kind)
				totalResources += backupCount
			}
		}
	}

	// 完成输出
	duration := time.Since(startTime).Round(time.Second)
	fmt.Printf("\n备份完成 🎉\n")
	fmt.Printf("总耗时: %s\n", duration)
	fmt.Printf("备份资源总数: %d\n", totalResources)
	fmt.Printf("备份位置: %s\n", backupRoot)
	fmt.Println("")
	fmt.Println("恢复说明:")
	fmt.Println("1. 恢复命名空间:")
	fmt.Printf("   kubectl apply -f %s/<namespace>/00-namespace.yaml\n", backupRoot)
	fmt.Println("2. 恢复资源:")
	fmt.Printf("   kubectl apply -f %s/<namespace>/<resource_type>/ --recursive\n", backupRoot)
	fmt.Println("3. 恢复集群级资源: (如有)")
	fmt.Printf("   kubectl apply -f %s/_global/ --recursive\n", backupRoot)
	fmt.Println("")
	fmt.Println("注意: 使用前建议检查备份文件内容")
}
