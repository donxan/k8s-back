package main

import (
	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"
	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var version string = "v2.3.0" // 优化后的版本号

// ResourceInfo 包含资源的完整定义
type ResourceInfo struct {
	Kind       string
	GVR        schema.GroupVersionResource
	Namespaced bool
}

// 资源类型映射表
var resourceMap = map[string]ResourceInfo{
	"configmaps": {
		Kind: "ConfigMap",
		GVR: schema.GroupVersionResource{
			Group: "", Version: "v1", Resource: "configmaps",
		},
		Namespaced: true,
	},
	"deployments": {
		Kind: "Deployment",
		GVR: schema.GroupVersionResource{
			Group: "apps", Version: "v1", Resource: "deployments",
		},
		Namespaced: true,
	},
	"secrets": {
		Kind: "Secret",
		GVR: schema.GroupVersionResource{
			Group: "", Version: "v1", Resource: "secrets",
		},
		Namespaced: true,
	},
	"services": {
		Kind: "Service",
		GVR: schema.GroupVersionResource{
			Group: "", Version: "v1", Resource: "services",
		},
		Namespaced: true,
	},
	"persistentvolumeclaims": {
		Kind: "PersistentVolumeClaim",
		GVR: schema.GroupVersionResource{
			Group: "", Version: "v1", Resource: "persistentvolumeclaims",
		},
		Namespaced: true,
	},
	"statefulsets": {
		Kind: "StatefulSet",
		GVR: schema.GroupVersionResource{
			Group: "apps", Version: "v1", Resource: "statefulsets",
		},
		Namespaced: true,
	},
	"horizontalpodautoscalers": {
		Kind: "HorizontalPodAutoscaler",
		GVR: schema.GroupVersionResource{
			Group: "autoscaling", Version: "v2", Resource: "horizontalpodautoscalers",
		},
		Namespaced: true,
	},
	"cronjobs": {
		Kind: "CronJob",
		GVR: schema.GroupVersionResource{
			Group: "batch", Version: "v1", Resource: "cronjobs",
		},
		Namespaced: true,
	},
	"jobs": {
		Kind: "Job",
		GVR: schema.GroupVersionResource{
			Group: "batch", Version: "v1", Resource: "jobs",
		},
		Namespaced: true,
	},
	"persistentvolumes": {
		Kind: "PersistentVolume",
		GVR: schema.GroupVersionResource{
			Group: "", Version: "v1", Resource: "persistentvolumes",
		},
		Namespaced: false,
	},
	"serviceaccounts": {
		Kind: "ServiceAccount",
		GVR: schema.GroupVersionResource{
			Group: "", Version: "v1", Resource: "serviceaccounts",
		},
		Namespaced: true,
	},
	"ingresses": {
		Kind: "Ingress",
		GVR: schema.GroupVersionResource{
			Group: "networking.k8s.io", Version: "v1", Resource: "ingresses",
		},
		Namespaced: true,
	},
}

// CleanResource 清理资源中对恢复无用或有害的字段
func CleanResource(resource map[string]interface{}) map[string]interface{} {
	if resource == nil {
		return nil
	}

	// 移除顶层状态信息
	delete(resource, "status")

	// --- 递归清理函数定义 ---
	// 定义一个可重用的函数来清理任何 metadata 块
	var cleanMetadata func(map[string]interface{})
	cleanMetadata = func(metadata map[string]interface{}) {
		if metadata == nil {
			return
		}

		// 移除所有由Kubernetes自动生成的元数据字段
		for _, field := range []string{
			"creationTimestamp", "resourceVersion", "selfLink", "uid",
			"managedFields", "generation",
		} {
			delete(metadata, field)
		}

		// 清理annotations
		if annotations, ok := metadata["annotations"].(map[string]interface{}); ok {
			// 将需要移除的 annotations key 加入列表
			for _, keyToRemove := range []string{
				"kubectl.kubernetes.io/last-applied-configuration",
				"deployment.kubernetes.io/revision",
				"kubesphere.io/restartedAt",
				"logging.kubesphere.io/logsidecar-config",
			} {
				delete(annotations, keyToRemove)
			}
			// 如果清理后为空，则移除整个annotations字段
			if len(annotations) == 0 {
				delete(metadata, "annotations")
			}
		}
	}

	// 清理顶层 metadata
	if metadata, ok := resource["metadata"].(map[string]interface{}); ok {
		cleanMetadata(metadata)
	}

	// 清理 Pod 模板中的 metadata
	if spec, ok := resource["spec"].(map[string]interface{}); ok {
		// 清理 Deployment, StatefulSet, Job 等资源的 template.metadata
		if template, ok := spec["template"].(map[string]interface{}); ok {
			if templateMetadata, ok := template["metadata"].(map[string]interface{}); ok {
				cleanMetadata(templateMetadata) // 复用清理函数
			}
		}
		// 清理 CronJob 资源的 jobTemplate.spec.template.metadata
		if jobTemplate, ok := spec["jobTemplate"].(map[string]interface{}); ok {
			if jobSpec, ok := jobTemplate["spec"].(map[string]interface{}); ok {
				if template, ok := jobSpec["template"].(map[string]interface{}); ok {
					if templateMetadata, ok := template["metadata"].(map[string]interface{}); ok {
						cleanMetadata(templateMetadata) // 复用清理函数
					}
				}
			}
		}
	}

	// 根据资源类型进行特定字段的清理
	kind, _ := resource["kind"].(string)
	if spec, specOK := resource["spec"].(map[string]interface{}); specOK {
		switch kind {
		case "Service":
			for _, field := range []string{"clusterIP", "clusterIPs", "ipFamilies", "ipFamilyPolicy"} {
				delete(spec, field)
			}
		case "PersistentVolume":
			delete(spec, "claimRef")
		case "PersistentVolumeClaim":
			delete(spec, "volumeName")
		case "ServiceAccount":
			delete(resource, "secrets")
		}
	}

	return resource
}

// ShouldBackupSecret 判断Secret是否需要备份，过滤掉系统生成的Secret
func ShouldBackupSecret(secretObj map[string]interface{}) bool {
	metadata, ok := secretObj["metadata"].(map[string]interface{})
	if !ok {
		return false
	}
	name, _ := metadata["name"].(string)
	secretType, _ := secretObj["type"].(string)

	// 跳过由各类控制器或系统默认生成的Secret
	if strings.HasPrefix(name, "default-token-") ||
		strings.HasPrefix(name, "sh.helm.release.v1.") ||
		(strings.Contains(name, "-token-") && secretType == string(corev1.SecretTypeServiceAccountToken)) {
		return false
	}

	// 跳过特定类型的Secret
	excludedTypes := map[string]struct{}{
		string(corev1.SecretTypeServiceAccountToken): {},
		"helm.sh/release.v1":                         {},
	}
	if _, found := excludedTypes[secretType]; found {
		return false
	}

	return true
}

// processStringMapValues 标准化ConfigMap中的字符串值，处理换行和转义
func processStringMapValues(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	processed := make(map[string]interface{})
	for k, v := range m {
		if s, ok := v.(string); ok {
			s = strings.ReplaceAll(s, "\r\n", "\n")
			s = strings.ReplaceAll(s, "\\n", "\n")
			processed[k] = s
		} else {
			processed[k] = v
		}
	}
	return processed
}

// checkResourceAccess 检查当前用户是否有指定资源的读取权限
func checkResourceAccess(clientset *kubernetes.Clientset, gvr schema.GroupVersionResource, namespace string) bool {
	ssar := &authv1.SelfSubjectAccessReview{
		Spec: authv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authv1.ResourceAttributes{
				Group: gvr.Group, Version: gvr.Version, Resource: gvr.Resource,
				Verb: "list", Namespace: namespace,
			},
		},
	}

	result, err := clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(
		context.TODO(), ssar, metav1.CreateOptions{})
	if err != nil {
		fmt.Printf("权限检查API调用失败 [%s in %s]: %v\n", gvr.Resource, namespace, err)
		return false
	}
	return result.Status.Allowed
}

func main() {
	var kubeconfig, namespace, resourceTypesStr, outputDir, skipNamespacesStr string
	var showVersion, skipSecrets, skipClusterResources bool

	pflag.StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig文件路径 (默认使用~/.kube/config)")
	pflag.StringVarP(&namespace, "namespace", "n", "all", "指定备份的命名空间 (使用'all'备份所有)")
	pflag.StringVarP(&resourceTypesStr, "type", "t", "all", "备份的资源类型 (逗号分隔, 'all'代表所有支持的类型)")
	pflag.StringVarP(&outputDir, "output-dir", "o", ".", "备份文件的输出目录")
	pflag.StringVarP(&skipNamespacesStr, "exclude-namespaces", "e", "kube-system", "需要排除的命名空间 (逗号分隔)")
	pflag.BoolVar(&skipSecrets, "skip-secrets", false, "跳过所有Secret的备份")
	pflag.BoolVar(&skipClusterResources, "no-cluster-resources", false, "不备份所有集群级资源 (如PV)")
	pflag.BoolVarP(&showVersion, "version", "v", false, "显示工具版本号")
	pflag.Parse()

	if showVersion {
		fmt.Printf("k8s-backup-tool %s\n", version)
		os.Exit(0)
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: 无法加载Kubernetes配置: %v\n", err)
		os.Exit(1)
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: 创建动态客户端失败: %v\n", err)
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: 创建标准客户端失败: %v\n", err)
		os.Exit(1)
	}

	skipNamespaces := strings.Split(skipNamespacesStr, ",")
	timestamp := time.Now().Format("20060102-150405")
	backupRoot := filepath.Join(outputDir, fmt.Sprintf("k8s-backup-%s", timestamp))
	if err := os.MkdirAll(backupRoot, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "错误: 创建备份目录 '%s' 失败: %v\n", backupRoot, err)
		os.Exit(1)
	}

	fmt.Printf("备份开始于: %s\n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Printf("备份目录: %s\n", backupRoot)

	var resourceTypes []string
	if resourceTypesStr == "all" || resourceTypesStr == "" {
		for resType := range resourceMap {
			resourceTypes = append(resourceTypes, resType)
		}
	} else {
		resourceTypes = strings.Split(resourceTypesStr, ",")
	}
	fmt.Printf("备份资源类型: %v\n", resourceTypes)

	var targetNamespaces []string
	if namespace == "all" {
		nsList, err := clientset.CoreV1().Namespaces().List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "警告: 获取命名空间列表失败: %v\n", err)
		} else {
			nsLookup := make(map[string]struct{})
			for _, ns := range skipNamespaces {
				nsLookup[ns] = struct{}{}
			}
			for _, ns := range nsList.Items {
				if _, found := nsLookup[ns.Name]; !found {
					targetNamespaces = append(targetNamespaces, ns.Name)
				}
			}
		}
	} else {
		targetNamespaces = []string{namespace}
	}
	fmt.Printf("目标命名空间: %v\n", targetNamespaces)

	totalResources := 0
	startTime := time.Now()

	for _, nsName := range targetNamespaces {
		fmt.Printf("\n[命名空间: %s]\n", nsName)
		nsDir := filepath.Join(backupRoot, nsName)
		if err := os.MkdirAll(nsDir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "  警告: 创建目录 '%s' 失败: %v\n", nsDir, err)
			continue
		}

		nsResource := map[string]interface{}{
			"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]string{"name": nsName},
		}
		nsYaml, _ := yaml.Marshal(nsResource)
		os.WriteFile(filepath.Join(nsDir, "00-namespace.yaml"), nsYaml, 0644)

		for _, resType := range resourceTypes {
			resInfo, exists := resourceMap[resType]
			if !exists || !resInfo.Namespaced {
				continue
			}
			if skipSecrets && resType == "secrets" {
				continue
			}
			if !checkResourceAccess(clientset, resInfo.GVR, nsName) {
				fmt.Printf("  警告: 无权限读取 %s, 跳过\n", resInfo.Kind)
				continue
			}

			resClient := dynamicClient.Resource(resInfo.GVR).Namespace(nsName)
			resList, err := resClient.List(context.TODO(), metav1.ListOptions{})
			if err != nil {
				fmt.Fprintf(os.Stderr, "  错误: 获取 %s 失败: %v\n", resInfo.Kind, err)
				continue
			}
			if len(resList.Items) == 0 {
				continue
			}
			fmt.Printf("  资源: %s (找到 %d 个)\n", resInfo.Kind, len(resList.Items))

			resources := resList.Items
			if resType == "secrets" {
				var filtered []unstructured.Unstructured
				for _, r := range resources {
					if ShouldBackupSecret(r.Object) {
						filtered = append(filtered, r)
					}
				}
				resources = filtered
			}
			if len(resources) == 0 {
				continue
			}

			resDir := filepath.Join(nsDir, resType)
			os.MkdirAll(resDir, 0755)

			backupCount := 0
			for _, resource := range resources {
				obj := CleanResource(resource.Object)
				if resType == "configmaps" {
					if data, ok := obj["data"].(map[string]interface{}); ok {
						obj["data"] = processStringMapValues(data)
					}
				}

				yamlData, err := yaml.Marshal(obj)
				if err != nil {
					fmt.Fprintf(os.Stderr, "    错误: 序列化 '%s' 失败: %v\n", resource.GetName(), err)
					continue
				}

				filename := fmt.Sprintf("%s.yaml", resource.GetName())
				fullPath := filepath.Join(resDir, filename)
				if err := os.WriteFile(fullPath, yamlData, 0644); err != nil {
					fmt.Fprintf(os.Stderr, "    错误: 写入文件 '%s' 失败: %v\n", fullPath, err)
					continue
				}
				backupCount++
			}
			fmt.Printf("    ✓ 备份 %d 个 %s\n", backupCount, resInfo.Kind)
			totalResources += backupCount
		}
	}

	if !skipClusterResources {
		fmt.Println("\n[集群范围资源]")
		globalDir := filepath.Join(backupRoot, "_global")
		os.MkdirAll(globalDir, 0755)

		for _, resType := range resourceTypes {
			resInfo, exists := resourceMap[resType]
			if !exists || resInfo.Namespaced {
				continue
			}
			if !checkResourceAccess(clientset, resInfo.GVR, "") {
				fmt.Printf("  警告: 无权限读取集群级 %s, 跳过\n", resInfo.Kind)
				continue
			}

			resClient := dynamicClient.Resource(resInfo.GVR)
			resList, err := resClient.List(context.TODO(), metav1.ListOptions{})
			if err != nil {
				fmt.Fprintf(os.Stderr, "  错误: 获取 %s 失败: %v\n", resInfo.Kind, err)
				continue
			}
			if len(resList.Items) == 0 {
				continue
			}
			fmt.Printf("  资源: %s (找到 %d 个)\n", resInfo.Kind, len(resList.Items))

			resDir := filepath.Join(globalDir, resType)
			os.MkdirAll(resDir, 0755)

			backupCount := 0
			for _, resource := range resList.Items {
				obj := CleanResource(resource.Object)
				yamlData, err := yaml.Marshal(obj)
				if err != nil {
					fmt.Fprintf(os.Stderr, "    错误: 序列化 '%s' 失败: %v\n", resource.GetName(), err)
					continue
				}
				filename := fmt.Sprintf("%s.yaml", resource.GetName())
				fullPath := filepath.Join(resDir, filename)
				if err := os.WriteFile(fullPath, yamlData, 0644); err != nil {
					fmt.Fprintf(os.Stderr, "    错误: 写入文件 '%s' 失败: %v\n", fullPath, err)
					continue
				}
				backupCount++
			}
			fmt.Printf("    ✓ 备份 %d 个 %s\n", backupCount, resInfo.Kind)
			totalResources += backupCount
		}
	}

	duration := time.Since(startTime).Round(time.Second)
	fmt.Printf("\n备份完成 🎉\n")
	fmt.Printf("总耗时: %s\n", duration)
	fmt.Printf("备份资源总数: %d\n", totalResources)
	fmt.Printf("备份位置: %s\n\n", backupRoot)
	fmt.Println("恢复说明:")
	fmt.Println("1. 恢复命名空间 (如果需要):")
	fmt.Printf("   kubectl apply -f %s/<namespace>/00-namespace.yaml\n", backupRoot)
	fmt.Println("2. 恢复命名空间内资源:")
	fmt.Printf("   kubectl apply -n <namespace> -f %s/<namespace>/\n", backupRoot)
	fmt.Println("3. 恢复集群级资源 (如有):")
	fmt.Printf("   kubectl apply -f %s/_global/\n", backupRoot)
	fmt.Println("\n注意: 恢复前请务必检查备份文件的内容，特别是存储和网络相关的配置。")
}
