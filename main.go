package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/gin-contrib/static"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type ClusterConfig struct {
	Server     string
	BearerToken string
	CAData      []byte
	IsInCluster bool
}

type ClusterCredentialManager struct {
	clientset         *kubernetes.Clientset
	argocdNamespace   string
}

func NewClusterCredentialManager() (*ClusterCredentialManager, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create clientset: %w", err)
	}

	argocdNamespace := os.Getenv("ARGOCD_NAMESPACE")
	if argocdNamespace == "" {
		argocdNamespace = "argocd"
	}

	return &ClusterCredentialManager{
		clientset:       clientset,
		argocdNamespace: argocdNamespace,
	}, nil
}

func (m *ClusterCredentialManager) GetClusterConfig(clusterURL string, clusterName string) (*ClusterConfig, error) {
	if clusterURL == "" && clusterName == "" {
		return &ClusterConfig{IsInCluster: true}, nil
	}
	if clusterURL == "https://kubernetes.default.svc" {
		return &ClusterConfig{IsInCluster: true}, nil
	}

	secrets, err := m.clientset.CoreV1().Secrets(m.argocdNamespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: "argocd.argoproj.io/secret-type=cluster",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list cluster secrets: %w", err)
	}

	for _, secret := range secrets.Items {
		if clusterURL != "" && m.matchesClusterURL(&secret, clusterURL) {
			return m.parseClusterSecret(&secret)
		}
		if clusterName != "" && m.matchesClusterName(&secret, clusterName) {
			return m.parseClusterSecret(&secret)
		}
	}

	identifier := clusterURL
	if identifier == "" {
		identifier = clusterName
	}
	return nil, fmt.Errorf("cluster not found: %s", identifier)
}

func (m *ClusterCredentialManager) matchesClusterName(secret *corev1.Secret, clusterName string) bool {
	nameBytes, ok := secret.Data["name"]
	if !ok {
		return false
	}
	return string(nameBytes) == clusterName
}

func (m *ClusterCredentialManager) matchesClusterURL(secret *corev1.Secret, clusterURL string) bool {
	serverBytes, ok := secret.Data["server"]
	if !ok {
		return false
	}
	server := string(serverBytes)

	server = strings.TrimSuffix(server, "/")
	clusterURL = strings.TrimSuffix(clusterURL, "/")

	return server == clusterURL
}

func (m *ClusterCredentialManager) parseClusterSecret(secret *corev1.Secret) (*ClusterConfig, error) {
	server, ok := secret.Data["server"]
	if !ok {
		return nil, fmt.Errorf("cluster secret missing 'server' field")
	}

	config := &ClusterConfig{
		Server:      string(server),
		IsInCluster: false,
	}

	if token, ok := secret.Data["config"]; ok {
		tokenStr := string(token)

		if strings.Contains(tokenStr, "bearerToken") {
			parts := strings.Split(tokenStr, "\"bearerToken\":\"")
			if len(parts) > 1 {
				tokenParts := strings.Split(parts[1], "\"")
				if len(tokenParts) > 0 {
					config.BearerToken = tokenParts[0]
				}
			}
		}
	}

	if config.BearerToken == "" {
		if token, ok := secret.Data["token"]; ok {
			config.BearerToken = string(token)
		}
	}

	if caData, ok := secret.Data["config"]; ok {
		caStr := string(caData)
		if strings.Contains(caStr, "tlsClientConfig") && strings.Contains(caStr, "caData") {
			parts := strings.Split(caStr, "\"caData\":\"")
			if len(parts) > 1 {
				caParts := strings.Split(parts[1], "\"")
				if len(caParts) > 0 {
					decoded, err := base64.StdEncoding.DecodeString(caParts[0])
					if err == nil {
						config.CAData = decoded
					}
				}
			}
		}
	}

	if config.BearerToken == "" {
		return nil, fmt.Errorf("cluster secret missing authentication credentials")
	}

	return config, nil
}

// GenerateKubeconfigFile creates a temporary kubeconfig file for the cluster
func (m *ClusterCredentialManager) GenerateKubeconfigFile(config *ClusterConfig) (string, error) {
	if config.IsInCluster {
		// No kubeconfig needed for in-cluster access
		return "", nil
	}

	tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("kubeconfig-%s.yaml", uuid.New().String()))

	// Build kubeconfig content
	kubeconfigContent := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- cluster:
    server: %s
`, config.Server)

	if len(config.CAData) > 0 {
		kubeconfigContent += fmt.Sprintf("    certificate-authority-data: %s\n", base64.StdEncoding.EncodeToString(config.CAData))
	} else {
		kubeconfigContent += "    insecure-skip-tls-verify: true\n"
	}

	kubeconfigContent += fmt.Sprintf(`  name: target-cluster
contexts:
- context:
    cluster: target-cluster
    user: target-user
  name: target-context
current-context: target-context
users:
- name: target-user
  user:
    token: %s
`, config.BearerToken)

	// Write kubeconfig file with restricted permissions
	err := os.WriteFile(tmpFile, []byte(kubeconfigContent), 0600)
	if err != nil {
		return "", fmt.Errorf("failed to write kubeconfig: %w", err)
	}

	return tmpFile, nil
}

var (
	tmpFilePath string
	credManager *ClusterCredentialManager
)

func init() {
	tmpFilePath = os.Getenv("TMP_FILE_PATH")
	if tmpFilePath == "" {
		tmpFilePath = "argocd-extension-pod-files"
	}

	var err error
	credManager, err = NewClusterCredentialManager()
	if err != nil {
		fmt.Printf("Warning: Failed to initialize cluster credential manager: %v\n", err)
		fmt.Println("Multi-cluster support will be disabled. In-cluster operations will still work.")
	}
}

func executeKubectl(clusterConfig *ClusterConfig, args ...string) ([]byte, error) {
	cmd := exec.Command("kubectl", args...)

	if clusterConfig != nil && !clusterConfig.IsInCluster {
		kubeconfigFile, err := credManager.GenerateKubeconfigFile(clusterConfig)
		if err != nil {
			return nil, fmt.Errorf("failed to generate kubeconfig: %w", err)
		}
		defer os.Remove(kubeconfigFile)

		cmd.Env = append(os.Environ(), fmt.Sprintf("KUBECONFIG=%s", kubeconfigFile))
	}

	return cmd.CombinedOutput()
}

func main() {
	r := gin.New()

	r.Use(
		gin.LoggerWithConfig(gin.LoggerConfig{
			SkipPaths: []string{"/"},
		}),
		gin.Recovery(),
	)

	r.GET("/", func(c *gin.Context) {
		c.String(http.StatusOK, "OK")
	})

	r.GET("/files", func(c *gin.Context) {
		namespace := c.DefaultQuery("namespace", "")
		pod := c.DefaultQuery("pod", "")
		container := c.DefaultQuery("container", "")
		filePath := c.DefaultQuery("path", "")
		clusterURL := c.DefaultQuery("clusterUrl", "")
		clusterName := c.DefaultQuery("clusterName", "")

		var clusterConfig *ClusterConfig
		var err error
		if credManager != nil {
			clusterConfig, err = credManager.GetClusterConfig(clusterURL, clusterName)
			if err != nil {
				c.JSON(http.StatusNotFound, gin.H{
					"error": fmt.Sprintf("Failed to get cluster config: %v", err),
					"hint":  "Ensure the cluster is registered in ArgoCD",
				})
				return
			}
		}

		tmpFilePath := path.Join(os.TempDir(), tmpFilePath, uuid.New().String(), filepath.Base(filePath))

		// kubectl cp <some-namespace>/<some-pod>:/tmp/foo /tmp/bar
		b, err := executeKubectl(clusterConfig, "cp", fmt.Sprintf("%s/%s:%s", namespace, pod, filePath), tmpFilePath, "-c", container)
		if err != nil {
			defer os.Remove(filepath.Dir(tmpFilePath))
			c.JSON(http.StatusInternalServerError, gin.H{
				"error":  fmt.Sprintf("kubectl cp exec error: %v", err),
				"output": string(b),
			})
			return
		}
		defer os.Remove(filepath.Dir(tmpFilePath))

		c.File(tmpFilePath)
	})

	r.POST("/files", func(c *gin.Context) {
		namespace := c.DefaultQuery("namespace", "")
		pod := c.DefaultQuery("pod", "")
		container := c.DefaultQuery("container", "")
		filePath := c.DefaultQuery("path", "")
		clusterURL := c.DefaultQuery("clusterUrl", "")
		clusterName := c.DefaultQuery("clusterName", "")

		var clusterConfig *ClusterConfig
		var err error
		if credManager != nil {
			clusterConfig, err = credManager.GetClusterConfig(clusterURL, clusterName)
			if err != nil {
				c.JSON(http.StatusNotFound, gin.H{
					"error": fmt.Sprintf("Failed to get cluster config: %v", err),
					"hint":  "Ensure the cluster is registered in ArgoCD",
				})
				return
			}
		}

		file, err := c.FormFile("file")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Failed to get file: %v", err)})
			return
		}

		tmpFilePath := path.Join(os.TempDir(), tmpFilePath, uuid.New().String(), filepath.Base(filePath))
		if err := c.SaveUploadedFile(file, tmpFilePath); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to save file: %v", err)})
			return
		}

		// kubectl cp /tmp/foo <some-namespace>/<some-pod>:/tmp/bar
		b, err := executeKubectl(clusterConfig, "cp", tmpFilePath, fmt.Sprintf("%s/%s:%s", namespace, pod, filePath), "-c", container)
		if err != nil {
			defer os.Remove(filepath.Dir(tmpFilePath))
			c.JSON(http.StatusInternalServerError, gin.H{
				"error":  fmt.Sprintf("kubectl cp exec error: %v", err),
				"output": string(b),
			})
			return
		}
		defer os.Remove(filepath.Dir(tmpFilePath))

		c.String(http.StatusCreated, "Uploaded")
	})

	r.Use(static.Serve("/ui", static.LocalFile("ui", true)))

	r.Run()
}
