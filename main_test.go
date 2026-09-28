package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func clusterSecret(name, server string, data map[string]string) *corev1.Secret {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Data:       map[string][]byte{"name": []byte(name), "server": []byte(server)},
	}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	return s
}

func TestParseClusterSecret(t *testing.T) {
	ca, cert, key := b64("CA-PEM"), b64("CERT-PEM"), b64("KEY-PEM")

	tests := []struct {
		name      string
		config    string
		wantToken string
		wantCert  bool
		wantErr   string
	}{
		{
			name:      "bearer token, compact json as written by argocd cluster add",
			config:    `{"bearerToken":"tok","tlsClientConfig":{"insecure":false,"caData":"` + ca + `"}}`,
			wantToken: "tok",
		},
		{
			name:      "bearer token, pretty-printed json",
			config:    "{\n  \"bearerToken\": \"tok\",\n  \"tlsClientConfig\": {\n    \"caData\": \"" + ca + "\"\n  }\n}\n",
			wantToken: "tok",
		},
		{
			name:     "client certificate only",
			config:   `{"tlsClientConfig":{"insecure":false,"caData":"` + ca + `","certData":"` + cert + `","keyData":"` + key + `"}}`,
			wantCert: true,
		},
		{
			name:      "bearer token and client certificate",
			config:    `{"bearerToken":"tok","tlsClientConfig":{"caData":"` + ca + `","certData":"` + cert + `","keyData":"` + key + `"}}`,
			wantToken: "tok",
			wantCert:  true,
		},
		{
			name:    "empty bearer token with ca only",
			config:  `{"bearerToken":"","tlsClientConfig":{"caData":"` + ca + `"}}`,
			wantErr: "bearerToken (empty)",
		},
		{
			name:    "certificate without key",
			config:  `{"tlsClientConfig":{"certData":"` + cert + `"}}`,
			wantErr: "found: certData",
		},
		{
			name:    "exec provider is reported as unsupported",
			config:  `{"execProviderConfig":{"command":"argocd-k8s-auth"},"tlsClientConfig":{"caData":"` + ca + `"}}`,
			wantErr: "execProviderConfig [unsupported]",
		},
		{
			name:    "empty config object",
			config:  `{}`,
			wantErr: "no credential fields found",
		},
		{
			name:    "unparsable json",
			config:  `{"bearerToken":`,
			wantErr: "unparsable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secret := clusterSecret("cluster-x", "https://10.0.0.1:6443", map[string]string{"config": tt.config})
			got, err := parseClusterSecret(secret)

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Server != "https://10.0.0.1:6443" {
				t.Errorf("server = %q", got.Server)
			}
			if got.BearerToken != tt.wantToken {
				t.Errorf("bearer token = %q, want %q", got.BearerToken, tt.wantToken)
			}
			hasCert := len(got.CertData) > 0 && len(got.KeyData) > 0
			if hasCert != tt.wantCert {
				t.Errorf("has client cert = %v, want %v", hasCert, tt.wantCert)
			}
			if hasCert && (string(got.CertData) != "CERT-PEM" || string(got.KeyData) != "KEY-PEM") {
				t.Errorf("cert/key were not base64-decoded: %q / %q", got.CertData, got.KeyData)
			}
			if string(got.CAData) != "CA-PEM" {
				t.Errorf("ca = %q, want decoded PEM", got.CAData)
			}
		})
	}
}

func TestParseClusterSecretLegacyTokenKey(t *testing.T) {
	secret := clusterSecret("cluster-legacy", "https://10.0.0.2:6443", map[string]string{"token": "legacy-tok\n"})
	got, err := parseClusterSecret(secret)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.BearerToken != "legacy-tok" {
		t.Errorf("bearer token = %q", got.BearerToken)
	}
}

func TestParseClusterSecretMissingServer(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "broken"}, Data: map[string][]byte{"config": []byte(`{"bearerToken":"tok"}`)}}
	if _, err := parseClusterSecret(secret); err == nil || !strings.Contains(err.Error(), "'server'") {
		t.Fatalf("want missing-server error, got %v", err)
	}
}

func TestParseClusterSecretErrorDoesNotLeakSecrets(t *testing.T) {
	cert := b64("VERY-SECRET-CERT")
	secret := clusterSecret("cluster-x", "https://10.0.0.1:6443", map[string]string{
		"config": `{"tlsClientConfig":{"certData":"` + cert + `"},"password":"hunter2"}`,
	})
	_, err := parseClusterSecret(secret)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, leak := range []string{cert, "VERY-SECRET-CERT", "hunter2"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error leaks secret material: %s", err)
		}
	}
}

func readKubeconfig(t *testing.T, path string) (cluster map[string]any, user map[string]any) {
	t.Helper()
	t.Cleanup(func() { os.Remove(path) })

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("kubeconfig permissions = %o, want 600", perm)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var cfg struct {
		APIVersion     string `json:"apiVersion"`
		Kind           string `json:"kind"`
		CurrentContext string `json:"current-context"`
		Clusters       []struct {
			Cluster map[string]any `json:"cluster"`
		} `json:"clusters"`
		Users []struct {
			User map[string]any `json:"user"`
		} `json:"users"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("kubeconfig is not valid json: %v", err)
	}
	if cfg.APIVersion != "v1" || cfg.Kind != "Config" || cfg.CurrentContext != "target-context" {
		t.Errorf("unexpected kubeconfig header: %+v", cfg)
	}
	if len(cfg.Clusters) != 1 || len(cfg.Users) != 1 {
		t.Fatalf("want exactly one cluster and one user, got %d/%d", len(cfg.Clusters), len(cfg.Users))
	}
	return cfg.Clusters[0].Cluster, cfg.Users[0].User
}

func TestGenerateKubeconfigFileInCluster(t *testing.T) {
	path, err := generateKubeconfigFile(&ClusterConfig{IsInCluster: true})
	if err != nil || path != "" {
		t.Fatalf("in-cluster should produce no file, got %q, %v", path, err)
	}
}

func TestGenerateKubeconfigFileClientCert(t *testing.T) {
	path, err := generateKubeconfigFile(&ClusterConfig{
		Server:   "https://10.4.3.204:6443",
		CAData:   []byte("CA-PEM"),
		CertData: []byte("CERT-PEM"),
		KeyData:  []byte("KEY-PEM"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cluster, user := readKubeconfig(t, path)

	if cluster["server"] != "https://10.4.3.204:6443" {
		t.Errorf("server = %v", cluster["server"])
	}
	if cluster["certificate-authority-data"] != b64("CA-PEM") {
		t.Errorf("certificate-authority-data = %v", cluster["certificate-authority-data"])
	}
	if _, ok := cluster["insecure-skip-tls-verify"]; ok {
		t.Errorf("insecure-skip-tls-verify must not be set when a CA is present")
	}
	if user["client-certificate-data"] != b64("CERT-PEM") || user["client-key-data"] != b64("KEY-PEM") {
		t.Errorf("client cert/key not written: %v", user)
	}
	if _, ok := user["token"]; ok {
		t.Errorf("token must not be set for cert-only auth: %v", user)
	}
}

func TestGenerateKubeconfigFileBearerTokenWithoutCA(t *testing.T) {
	path, err := generateKubeconfigFile(&ClusterConfig{Server: "https://10.0.0.1:6443", BearerToken: "tok"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cluster, user := readKubeconfig(t, path)

	if user["token"] != "tok" {
		t.Errorf("token = %v", user["token"])
	}
	if cluster["insecure-skip-tls-verify"] != true {
		t.Errorf("no CA stored should skip verification, got %v", cluster)
	}
}

func TestGenerateKubeconfigFileInsecureDropsCA(t *testing.T) {
	path, err := generateKubeconfigFile(&ClusterConfig{Server: "https://10.0.0.1:6443", BearerToken: "tok", Insecure: true, CAData: []byte("CA-PEM")})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cluster, _ := readKubeconfig(t, path)

	if cluster["insecure-skip-tls-verify"] != true {
		t.Errorf("insecure flag not honoured: %v", cluster)
	}
	if _, ok := cluster["certificate-authority-data"]; ok {
		t.Errorf("kubectl rejects CA together with insecure; CA must be dropped: %v", cluster)
	}
}

func TestGenerateKubeconfigFileRefusesEmptyCredentials(t *testing.T) {
	if _, err := generateKubeconfigFile(&ClusterConfig{Server: "https://10.0.0.1:6443"}); err == nil {
		t.Fatal("expected an error for a config without credentials")
	}
}
