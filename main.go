package main

import (
	"context"
	"encoding/json"
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
	clientcmdv1 "k8s.io/client-go/tools/clientcmd/api/v1"
)

// ClusterConfig is what the backend needs to run kubectl against a target cluster.
type ClusterConfig struct {
	Server      string
	BearerToken string
	CAData      []byte
	CertData    []byte
	KeyData     []byte
	Insecure    bool
	ServerName  string
	IsInCluster bool
}

// HasCredentials reports whether the config carries a credential kubectl can use:
// a bearer token or a client certificate with its key.
func (c *ClusterConfig) HasCredentials() bool {
	return c.BearerToken != "" || (len(c.CertData) > 0 && len(c.KeyData) > 0)
}

// argoClusterConfig mirrors the JSON document Argo CD stores in the `config` key
// of a cluster secret (v1alpha1.ClusterConfig). []byte fields are base64 in JSON,
// exactly as Argo CD writes them, so encoding/json decodes them for us.
type argoClusterConfig struct {
	Username           string              `json:"username,omitempty"`
	Password           string              `json:"password,omitempty"`
	BearerToken        string              `json:"bearerToken,omitempty"`
	TLSClientConfig    argoTLSClientConfig `json:"tlsClientConfig"`
	AWSAuthConfig      json.RawMessage     `json:"awsAuthConfig,omitempty"`
	ExecProviderConfig json.RawMessage     `json:"execProviderConfig,omitempty"`
}

type argoTLSClientConfig struct {
	Insecure   bool   `json:"insecure"`
	ServerName string `json:"serverName,omitempty"`
	CAData     []byte `json:"caData,omitempty"`
	CertData   []byte `json:"certData,omitempty"`
	KeyData    []byte `json:"keyData,omitempty"`
}

type ClusterCredentialManager struct {
	clientset       *kubernetes.Clientset
	argocdNamespace string
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
			return parseClusterSecret(&secret)
		}
		if clusterName != "" && m.matchesClusterName(&secret, clusterName) {
			return parseClusterSecret(&secret)
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

// parseClusterSecret turns an Argo CD cluster secret into a ClusterConfig.
// It accepts a bearer token (`config.bearerToken` or the legacy `token` key) or a
// client certificate (`config.tlsClientConfig.certData` + `keyData`), which is
// what `argocd cluster add` stores when the kubectl context authenticates with
// x509 certs. The returned error names the fields that were present but never
// their values.
func parseClusterSecret(secret *corev1.Secret) (*ClusterConfig, error) {
	server, ok := secret.Data["server"]
	if !ok {
		return nil, fmt.Errorf("cluster secret %q is missing the 'server' field", secret.Name)
	}

	config := &ClusterConfig{Server: string(server)}
	var notes []string

	if raw, ok := secret.Data["config"]; ok && strings.TrimSpace(string(raw)) != "" {
		var ac argoClusterConfig
		if err := json.Unmarshal(raw, &ac); err != nil {
			return nil, fmt.Errorf("cluster secret %q has an unparsable 'config' field: %w", secret.Name, err)
		}

		config.BearerToken = ac.BearerToken
		config.CAData = ac.TLSClientConfig.CAData
		config.CertData = ac.TLSClientConfig.CertData
		config.KeyData = ac.TLSClientConfig.KeyData
		config.Insecure = ac.TLSClientConfig.Insecure
		config.ServerName = ac.TLSClientConfig.ServerName

		if ac.BearerToken == "" && strings.Contains(string(raw), `"bearerToken"`) {
			notes = append(notes, "bearerToken (empty)")
		}
		if ac.Username != "" || ac.Password != "" {
			notes = append(notes, "username/password [unsupported]")
		}
		if isSetJSON(ac.AWSAuthConfig) {
			notes = append(notes, "awsAuthConfig [unsupported]")
		}
		if isSetJSON(ac.ExecProviderConfig) {
			notes = append(notes, "execProviderConfig [unsupported]")
		}
	}

	// Legacy layout: a bare token stored next to the server URL.
	if config.BearerToken == "" {
		if token, ok := secret.Data["token"]; ok {
			config.BearerToken = strings.TrimSpace(string(token))
		}
	}

	if !config.HasCredentials() {
		return nil, fmt.Errorf("cluster secret %q has no usable credentials (%s); supported: bearerToken, tlsClientConfig.certData+keyData",
			secret.Name, describeCredentials(config, notes))
	}

	return config, nil
}

func isSetJSON(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null"
}

// describeCredentials lists which credential-related fields a secret carries so a
// rejection is explainable without leaking secret material.
func describeCredentials(c *ClusterConfig, notes []string) string {
	var found []string
	if len(c.CertData) > 0 {
		found = append(found, "certData")
	}
	if len(c.KeyData) > 0 {
		found = append(found, "keyData")
	}
	if len(c.CAData) > 0 {
		found = append(found, "caData")
	}
	found = append(found, notes...)
	if len(found) == 0 {
		return "no credential fields found"
	}
	return "found: " + strings.Join(found, ", ")
}

// generateKubeconfigFile writes a temporary kubeconfig for the target cluster and
// returns its path. The file is JSON, which kubectl accepts as a kubeconfig, so
// nothing is hand-templated and no value needs quoting.
func generateKubeconfigFile(config *ClusterConfig) (string, error) {
	if config.IsInCluster {
		// No kubeconfig needed for in-cluster access
		return "", nil
	}
	if !config.HasCredentials() {
		return "", fmt.Errorf("cluster %s has no usable credentials", config.Server)
	}

	cluster := clientcmdv1.Cluster{
		Server:        config.Server,
		TLSServerName: config.ServerName,
	}
	switch {
	case config.Insecure:
		// kubectl refuses a CA together with the insecure flag, so send only the flag.
		cluster.InsecureSkipTLSVerify = true
	case len(config.CAData) > 0:
		cluster.CertificateAuthorityData = config.CAData
	default:
		// Earlier versions skipped verification whenever no CA was stored; keep
		// that so existing registrations without a CA keep working.
		cluster.InsecureSkipTLSVerify = true
	}

	kubeconfig := clientcmdv1.Config{
		APIVersion: "v1",
		Kind:       "Config",
		Clusters: []clientcmdv1.NamedCluster{
			{Name: "target-cluster", Cluster: cluster},
		},
		AuthInfos: []clientcmdv1.NamedAuthInfo{
			{Name: "target-user", AuthInfo: clientcmdv1.AuthInfo{
				Token:                 config.BearerToken,
				ClientCertificateData: config.CertData,
				ClientKeyData:         config.KeyData,
			}},
		},
		Contexts: []clientcmdv1.NamedContext{
			{Name: "target-context", Context: clientcmdv1.Context{Cluster: "target-cluster", AuthInfo: "target-user"}},
		},
		CurrentContext: "target-context",
	}

	data, err := json.Marshal(kubeconfig)
	if err != nil {
		return "", fmt.Errorf("failed to encode kubeconfig: %w", err)
	}

	tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("kubeconfig-%s.json", uuid.New().String()))
	if err := os.WriteFile(tmpFile, data, 0600); err != nil {
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
		kubeconfigFile, err := generateKubeconfigFile(clusterConfig)
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

		// Everything for this request lives in its own directory; remove the
		// whole directory afterwards (os.Remove on a non-empty dir silently fails
		// and leaked every downloaded file into /tmp).
		defer os.RemoveAll(filepath.Dir(tmpFilePath))

		// kubectl cp <some-namespace>/<some-pod>:/tmp/foo /tmp/bar
		// --retries makes kubectl resume from the last byte received when the
		// exec stream ends early (seen on k3s when the reader is slower than the
		// pod: the last few MB of a large file were dropped).
		b, err := executeKubectl(clusterConfig, "cp", fmt.Sprintf("%s/%s:%s", namespace, pod, filePath), tmpFilePath, "-c", container, "--retries=10")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error":  fmt.Sprintf("kubectl cp exec error: %v", err),
				"output": string(b),
			})
			return
		}

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
		defer os.RemoveAll(filepath.Dir(tmpFilePath))
		if err := c.SaveUploadedFile(file, tmpFilePath); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to save file: %v", err)})
			return
		}

		// kubectl cp /tmp/foo <some-namespace>/<some-pod>:/tmp/bar
		b, err := executeKubectl(clusterConfig, "cp", tmpFilePath, fmt.Sprintf("%s/%s:%s", namespace, pod, filePath), "-c", container)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error":  fmt.Sprintf("kubectl cp exec error: %v", err),
				"output": string(b),
			})
			return
		}

		c.String(http.StatusCreated, "Uploaded")
	})

	r.Use(static.Serve("/ui", static.LocalFile("ui", true)))

	r.Run()
}
