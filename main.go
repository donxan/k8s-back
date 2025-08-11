package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/pflag"
	yaml "gopkg.in/yaml.v3" // 确保使用 yaml.v3
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

// version variable will be injected by Makefile at compile time to display program version
var version string = "unknown" // Default value, if not injected via ldflags

// ResourceKindMap maps resource type (plural) to its Kind (singular)
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

// GroupVersionResourceMap maps resource type (plural) to its GroupVersionResource (GVR)
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

// CleanResource cleans unnecessary fields from the resource
func CleanResource(resource map[string]interface{}) map[string]interface{} {
	if resource == nil {
		return nil
	}

	// Make a deep copy to avoid modifying the original unstructured.Unstructured object directly
	cleanedResource := make(map[string]interface{})
	for k, v := range resource {
		cleanedResource[k] = v
	}

	metadata, ok := cleanedResource["metadata"].(map[string]interface{})
	if ok {
		// Clean fields in metadata
		for _, field := range []string{"creationTimestamp", "resourceVersion", "selfLink", "uid", "managedFields", "generation"} {
			delete(metadata, field)
		}

		// Clean specific annotations
		if annotations, ok := metadata["annotations"].(map[string]interface{}); ok {
			delete(annotations, "kubectl.kubernetes.io/last-applied-configuration")
			if len(annotations) == 0 {
				delete(metadata, "annotations")
			} else {
				metadata["annotations"] = annotations // Ensure update back
			}
		}

		// Clean empty fields
		for _, field := range []string{"annotations", "labels", "finalizers"} {
			if val, exists := metadata[field]; exists {
				if m, isMap := val.(map[string]interface{}); isMap && len(m) == 0 {
					delete(metadata, field)
				}
			}
		}
	}

	// Delete the entire status field
	delete(cleanedResource, "status")

	kind, _ := cleanedResource["kind"].(string)
	if kind == "Deployment" {
		if spec, ok := cleanedResource["spec"].(map[string]interface{}); ok {
			if selector, ok := spec["selector"].(map[string]interface{}); ok {
				if matchLabels, ok := selector["matchLabels"].(map[string]interface{}); ok {
					if template, ok := spec["template"].(map[string]interface{}); ok {
						if tmplMetadata, ok := template["metadata"].(map[string]interface{}); ok {
							if tmplLabels, ok := tmplMetadata["labels"].(map[string]interface{}); ok {
								// If matchLabels and template.metadata.labels are identical, delete matchLabels
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
			// If type is not NodePort, delete nodePort from ports
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

// mapsEqual compares two map[string]interface{} for equality
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

// ShouldBackupSecret determines if a Secret should be backed up
func ShouldBackupSecret(secretObj map[string]interface{}) bool {
	metadata, ok := secretObj["metadata"].(map[string]interface{})
	if !ok {
		return false
	}
	name, _ := metadata["name"].(string)
	secretType, _ := secretObj["type"].(string)

	// Exclude default tokens and docker config secrets, as well as Helm internal secrets
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

// processStringMapValues recursively processes string values in a map[string]interface{},
// replacing escaped characters with actual newlines.
func processStringMapValues(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	processedMap := make(map[string]interface{})
	for k, v := range m {
		if s, isString := v.(string); isString {
			// Convert Windows-style newlines to Unix-style
			s = strings.ReplaceAll(s, "\r\n", "\n")
			// Unescape literal "\n" and "\r" to actual newline and carriage return characters
			s = strings.ReplaceAll(s, "\\n", "\n")
			s = strings.ReplaceAll(s, "\\r", "\r")
			processedMap[k] = s
		} else if subMap, isMap := v.(map[string]interface{}); isMap {
			// Recursively process nested maps
			processedMap[k] = processStringMapValues(subMap)
		} else {
			// Non-string values remain unchanged
			processedMap[k] = v
		}
	}
	return processedMap
}

func main() {
	var kubeconfig string
	var namespace string
	var resourceTypesStr string
	var outputDir string
	var showVersion bool // Version flag

	pflag.StringVar(&kubeconfig, "kubeconfig", "", "(Optional) Path to the kubeconfig file. If not specified, default search order will be used (KUBECONFIG environment variable or ~/.kube/config).")
	pflag.StringVarP(&namespace, "namespace", "n", "all", "Specify the namespace to backup resources from (e.g., 'my-namespace'). Use 'all' (default) to backup resources from all namespaces.")
	pflag.StringVarP(&resourceTypesStr, "type", "t", "", "Specify one or more resource types to backup, separated by commas (e.g., 'deployments,secrets'). If not specified, all supported types will be backed up.")
	pflag.StringVarP(&outputDir, "output-dir", "o", ".", "Specify the root directory for backup files. Defaults to the current directory.")
	pflag.BoolVarP(&showVersion, "version", "v", false, "Display program version information.")
	pflag.Parse()

	// If --version or -v flag is specified, print version and exit
	if showVersion {
		fmt.Printf("Kubernetes Backup Tool Version: %s\n", version)
		return
	}

	// Build Kubeconfig configuration
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		fmt.Printf("Error: Failed to load Kubernetes configuration: %v\n", err)
		fmt.Println("\nPlease check the following to resolve configuration issues:")
		fmt.Println("  1. Confirm your Kubernetes cluster is running and accessible.")
		fmt.Println("  2. If running locally, ensure the kubeconfig file exists.")
		if kubeconfig != "" {
			fmt.Printf("     You specified path '%s' via --kubeconfig argument, please check if the file exists and is valid.\n", kubeconfig)
		} else {
			fmt.Println("     The program will try to find the kubeconfig file in the following default locations:")
			fmt.Println("       - The path specified by the 'KUBECONFIG' environment variable.")
			fmt.Println("       - The '.kube/config' file under your user's home directory (e.g., '%USERPROFILE%\\.kube\\config' on Windows).")
			fmt.Println("     If no valid kubeconfig is found in these locations, please manually specify the correct path using the '--kubeconfig' argument.")
		}
		fmt.Println("  3. You can use the 'kubectl cluster-info' command to test your Kubernetes connection and configuration.")
		os.Exit(1)
	}

	// Create dynamic client
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		fmt.Printf("Error: Failed to create Kubernetes dynamic client: %v\n", err)
		os.Exit(1)
	}

	// Construct the final backup root directory path
	finalBackupRoot := filepath.Join(outputDir, fmt.Sprintf("k8s-backup-%s", time.Now().Format("20060102150405")))
	err = os.MkdirAll(finalBackupRoot, os.ModePerm)
	if err != nil {
		fmt.Printf("Error: Failed to create backup root directory '%s': %v\n", finalBackupRoot, err)
		os.Exit(1)
	}

	// Determine resource types to backup
	var resourceTypesToBackup []string
	if resourceTypesStr != "" {
		resourceTypesToBackup = strings.Split(resourceTypesStr, ",")
	} else {
		for rType := range GroupVersionResourceMap {
			resourceTypesToBackup = append(resourceTypesToBackup, rType)
		}
	}

	totalBackedUpResources := 0

	// Process each resource type
	for _, resTypePlural := range resourceTypesToBackup {
		kindName := ResourceKindMap[resTypePlural]
		gvr, ok := GroupVersionResourceMap[resTypePlural]
		if !ok {
			fmt.Printf("Warning: Unsupported resource type '%s', skipping.\n", resTypePlural)
			continue
		}

		fmt.Printf("\n--- Processing %ss ---\n", kindName)

		var resClient dynamic.ResourceInterface
		if namespace == "all" {
			resClient = dynamicClient.Resource(gvr)
		} else {
			resClient = dynamicClient.Resource(gvr).Namespace(namespace)
		}

		unstructuredList, err := resClient.List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			fmt.Printf("Error: Failed to get %s resources: %v\n", resTypePlural, err)
			continue
		}

		resources := unstructuredList.Items
		if len(resources) == 0 {
			fmt.Printf("No %ss found in %s.\n", kindName, func() string {
				if namespace == "all" {
					return "all namespaces"
				}
				return namespace
			}())
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
				fmt.Printf("Filtered out %d internal Secrets.\n", initialSecretCount-len(resources))
			}
		}

		if len(resources) == 0 {
			fmt.Printf("No %ss to backup after filtering.\n", kindName)
			continue
		}

		backedUpCountForType := 0
		for _, resource := range resources {
			resourceMap := resource.Object

			cleaned := CleanResource(resourceMap)

			metadata, ok := cleaned["metadata"].(map[string]interface{})
			if !ok {
				fmt.Printf("Warning: Resource %s has no valid metadata, skipping.\n", kindName)
				continue
			}
			name, ok := metadata["name"].(string)
			if !ok {
				fmt.Printf("Warning: Resource %s has no valid name, skipping.\n", kindName)
				continue
			}

			namespaceDir := "_cluster_" // Default for cluster-scoped resources
			if ns, ok := metadata["namespace"].(string); ok && ns != "" {
				namespaceDir = ns
			}

			// Construct new directory structure: finalBackupRoot/namespace/resource_type/
			nsDir := filepath.Join(finalBackupRoot, namespaceDir)
			resourceTypeDir := filepath.Join(nsDir, resTypePlural)

			err = os.MkdirAll(resourceTypeDir, os.ModePerm) // Create resource type directory
			if err != nil {
				fmt.Printf("Error: Failed to create directory '%s': %v\n", resourceTypeDir, err)
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

			// Special handling for ConfigMap's data field
			if data, ok := cleaned["data"]; ok {
				if dataMap, isMap := data.(map[string]interface{}); isMap {
					outputData["data"] = processStringMapValues(dataMap) // Call new processing function
				} else {
					outputData["data"] = data
				}
			}
			// Special handling for Secret's stringData field (do NOT process Secret's data field, as it's typically Base64 encoded)
			if stringData, ok := cleaned["stringData"]; ok {
				if stringDataMap, isMap := stringData.(map[string]interface{}); isMap {
					outputData["stringData"] = processStringMapValues(stringDataMap) // Call new processing function
				} else {
					outputData["stringData"] = stringData
				}
			}
			if rules, ok := cleaned["rules"]; ok {
				outputData["rules"] = rules
			}

			yamlData, err := yaml.Marshal(outputData) // Use yaml.v3's Marshal
			if err != nil {
				fmt.Printf("Warning: Failed to marshal resource %s/%s to YAML: %v\n", namespaceDir, name, err)
				continue
			}

			filename := filepath.Join(resourceTypeDir, fmt.Sprintf("%s.yaml", name)) // Save file to resource type directory
			err = os.WriteFile(filename, yamlData, 0644)                             // Use os.WriteFile
			if err != nil {
				fmt.Printf("Warning: Failed to save file '%s': %v\n", filename, err)
				continue
			}
			backedUpCountForType++
		}
		fmt.Printf("Backed up %d %ss.\n", backedUpCountForType, kindName)
		totalBackedUpResources += backedUpCountForType
	}

	fmt.Printf("\n--- Backup Complete 🎉 ---\n")
	fmt.Printf("Backup directory: %s\n", finalBackupRoot)
	fmt.Printf("Total resources backed up: %d\n", totalBackedUpResources)
	fmt.Println("\nTo restore resources, navigate to the respective resource type and namespace directory, then apply the YAML files:")
	fmt.Println("  cd <Your Custom Dir>/k8s-backup-<DateTime>/<namespace>/<resource_type>/")
	fmt.Println("  kubectl apply -f <resource_name>.yaml")
}
