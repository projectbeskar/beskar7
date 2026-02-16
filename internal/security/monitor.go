package security

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrastructurev1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
)

// Security severity constants
const (
	SeverityCritical = "CRITICAL"
	SeverityHigh     = "HIGH"
	SeverityMedium   = "MEDIUM"
	SeverityLow      = "LOW"
)

// SecurityMonitor monitors security configurations and reports issues
type SecurityMonitor struct {
	client.Client
	KubernetesClient kubernetes.Interface
	TLSValidator     *TLSValidator
	RBACValidator    *RBACValidator
	Interval         time.Duration
	Namespace        string
}

// SecurityReport contains the results of a security scan
type SecurityReport struct {
	Timestamp       time.Time                `json:"timestamp"`
	TotalIssues     int                      `json:"total_issues"`
	CriticalIssues  int                      `json:"critical_issues"`
	HighIssues      int                      `json:"high_issues"`
	MediumIssues    int                      `json:"medium_issues"`
	LowIssues       int                      `json:"low_issues"`
	TLSFindings     []TLSSecurityFinding     `json:"tls_findings"`
	RBACFindings    []RBACSecurityFinding    `json:"rbac_findings"`
	SecretFindings  []SecretSecurityFinding  `json:"secret_findings"`
	GeneralFindings []GeneralSecurityFinding `json:"general_findings"`
	Recommendations []SecurityRecommendation `json:"recommendations"`
}

// TLSSecurityFinding represents a TLS-related security issue
type TLSSecurityFinding struct {
	Severity    string    `json:"severity"`
	Type        string    `json:"type"`
	Resource    string    `json:"resource"`
	Description string    `json:"description"`
	Details     string    `json:"details"`
	Remediation string    `json:"remediation"`
	Expiry      time.Time `json:"expiry,omitempty"`
}

// RBACSecurityFinding represents an RBAC-related security issue
type RBACSecurityFinding struct {
	Severity    string   `json:"severity"`
	Type        string   `json:"type"`
	Resource    string   `json:"resource"`
	Description string   `json:"description"`
	OverlyBroad bool     `json:"overly_broad"`
	Permissions []string `json:"permissions"`
	Remediation string   `json:"remediation"`
}

// SecretSecurityFinding represents a secret-related security issue
type SecretSecurityFinding struct {
	Severity    string        `json:"severity"`
	Type        string        `json:"type"`
	Resource    string        `json:"resource"`
	Description string        `json:"description"`
	Age         time.Duration `json:"age"`
	LastRotated time.Time     `json:"last_rotated,omitempty"`
	Remediation string        `json:"remediation"`
}

// GeneralSecurityFinding represents a general security issue
type GeneralSecurityFinding struct {
	Severity    string `json:"severity"`
	Type        string `json:"type"`
	Resource    string `json:"resource"`
	Description string `json:"description"`
	Remediation string `json:"remediation"`
}

// SecurityRecommendation provides actionable security recommendations
type SecurityRecommendation struct {
	Priority    string `json:"priority"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Action      string `json:"action"`
	Impact      string `json:"impact"`
	Effort      string `json:"effort"`
}

// NewSecurityMonitor creates a new security monitor
func NewSecurityMonitor(client client.Client, k8sClient kubernetes.Interface, namespace string) *SecurityMonitor {
	return &SecurityMonitor{
		Client:           client,
		KubernetesClient: k8sClient,
		TLSValidator:     NewTLSValidator(),
		RBACValidator:    NewRBACValidator(k8sClient),
		Interval:         1 * time.Hour, // Default scan every hour
		Namespace:        namespace,
	}
}

// StartMonitoring starts the security monitoring process
func (m *SecurityMonitor) StartMonitoring(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("security-monitor")
	logger.Info("Starting security monitoring", "interval", m.Interval)

	ticker := time.NewTicker(m.Interval)
	defer ticker.Stop()

	// Run initial scan
	m.runSecurityScan(ctx)

	for {
		select {
		case <-ctx.Done():
			logger.Info("Security monitoring stopped")
			return ctx.Err()
		case <-ticker.C:
			m.runSecurityScan(ctx)
		}
	}
}

// runSecurityScan performs a comprehensive security scan
func (m *SecurityMonitor) runSecurityScan(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("security-scan")
	logger.Info("Running security scan")

	report := &SecurityReport{
		Timestamp:       time.Now(),
		TLSFindings:     make([]TLSSecurityFinding, 0),
		RBACFindings:    make([]RBACSecurityFinding, 0),
		SecretFindings:  make([]SecretSecurityFinding, 0),
		GeneralFindings: make([]GeneralSecurityFinding, 0),
		Recommendations: make([]SecurityRecommendation, 0),
	}

	// Scan PhysicalHosts for TLS issues
	if err := m.scanPhysicalHostsTLS(ctx, report); err != nil {
		logger.Error(err, "Failed to scan PhysicalHosts for TLS issues")
	}

	// Scan RBAC configurations
	if err := m.scanRBACConfigurations(ctx, report); err != nil {
		logger.Error(err, "Failed to scan RBAC configurations")
	}

	// Scan secrets for security issues
	if err := m.scanSecrets(ctx, report); err != nil {
		logger.Error(err, "Failed to scan secrets")
	}

	// Generate recommendations
	m.generateRecommendations(report)

	// Calculate totals
	m.calculateTotals(report)

	// Report findings
	m.reportFindings(ctx, report)

	logger.Info("Security scan completed",
		"total_issues", report.TotalIssues,
		"critical", report.CriticalIssues,
		"high", report.HighIssues,
		"medium", report.MediumIssues,
		"low", report.LowIssues)

	// Scan completed
}

// scanPhysicalHostsTLS scans PhysicalHosts for TLS security issues
func (m *SecurityMonitor) scanPhysicalHostsTLS(ctx context.Context, report *SecurityReport) error {
	physicalHosts := &infrastructurev1beta1.PhysicalHostList{}
	if err := m.List(ctx, physicalHosts, client.InNamespace(m.Namespace)); err != nil {
		return fmt.Errorf("failed to list PhysicalHosts: %w", err)
	}

	for _, host := range physicalHosts.Items {
		// Check for insecureSkipVerify usage
		if host.Spec.RedfishConnection.InsecureSkipVerify != nil && *host.Spec.RedfishConnection.InsecureSkipVerify {
			report.TLSFindings = append(report.TLSFindings, TLSSecurityFinding{
				Severity:    "MEDIUM",
				Type:        "InsecureSkipVerify",
				Resource:    fmt.Sprintf("PhysicalHost/%s", host.Name),
				Description: "TLS certificate verification is disabled",
				Details:     "insecureSkipVerify is set to true",
				Remediation: "Configure proper TLS certificates or use a custom CA bundle",
			})
		}

		// Validate TLS certificate if possible
		if host.Spec.RedfishConnection.Address != "" {
			result, err := m.TLSValidator.ValidateTLSEndpoint(ctx, host.Spec.RedfishConnection.Address)
			if err == nil && result != nil {
				for _, warning := range result.Warnings {
					report.TLSFindings = append(report.TLSFindings, TLSSecurityFinding{
						Severity:    "LOW",
						Type:        "TLSWarning",
						Resource:    fmt.Sprintf("PhysicalHost/%s", host.Name),
						Description: warning,
						Details:     fmt.Sprintf("Endpoint: %s", host.Spec.RedfishConnection.Address),
						Remediation: "Review certificate configuration",
						Expiry:      result.Expiry,
					})
				}

				for _, error := range result.Errors {
					severity := SeverityHigh
					if result.IsSelfSigned {
						severity = SeverityMedium
					}
					report.TLSFindings = append(report.TLSFindings, TLSSecurityFinding{
						Severity:    severity,
						Type:        "TLSError",
						Resource:    fmt.Sprintf("PhysicalHost/%s", host.Name),
						Description: error,
						Details:     fmt.Sprintf("Endpoint: %s", host.Spec.RedfishConnection.Address),
						Remediation: "Fix certificate configuration or use proper CA-signed certificates",
						Expiry:      result.Expiry,
					})
				}
			}
		}
	}

	return nil
}

// scanRBACConfigurations scans RBAC configurations for security issues
func (m *SecurityMonitor) scanRBACConfigurations(ctx context.Context, report *SecurityReport) error {
	// Scan ClusterRoles
	clusterRoles, err := m.KubernetesClient.RbacV1().ClusterRoles().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list ClusterRoles: %w", err)
	}

	for _, clusterRole := range clusterRoles.Items {
		// Focus on Beskar7-related roles
		if !m.isBeskar7Related(clusterRole.Name) {
			continue
		}

		result := m.RBACValidator.ValidateClusterRole(ctx, &clusterRole)

		for _, finding := range result.SecurityFindings {
			report.RBACFindings = append(report.RBACFindings, RBACSecurityFinding{
				Severity:    finding.Severity,
				Type:        finding.Type,
				Resource:    fmt.Sprintf("ClusterRole/%s", clusterRole.Name),
				Description: finding.Description,
				OverlyBroad: finding.Type == "ClusterAdminEquivalent" || finding.Type == "OverlyBroadVerbs",
				Permissions: m.extractPermissions(&clusterRole),
				Remediation: finding.Remediation,
			})
		}

		for _, rule := range result.OverlyBroadRules {
			report.RBACFindings = append(report.RBACFindings, RBACSecurityFinding{
				Severity:    rule.Severity,
				Type:        "OverlyBroadRule",
				Resource:    fmt.Sprintf("ClusterRole/%s", clusterRole.Name),
				Description: rule.Reason,
				OverlyBroad: true,
				Permissions: append(append(rule.APIGroups, rule.Resources...), rule.Verbs...),
				Remediation: "Apply principle of least privilege",
			})
		}
	}

	return nil
}

// scanSecrets scans secrets for security issues
func (m *SecurityMonitor) scanSecrets(ctx context.Context, report *SecurityReport) error {
	secrets := &corev1.SecretList{}
	if err := m.List(ctx, secrets, client.InNamespace(m.Namespace)); err != nil {
		return fmt.Errorf("failed to list secrets: %w", err)
	}

	for _, secret := range secrets.Items {
		// Focus on credential secrets
		if !m.isCredentialSecret(&secret) {
			continue
		}

		age := time.Since(secret.CreationTimestamp.Time)

		// Check for old secrets (>90 days)
		if age > 90*24*time.Hour {
			report.SecretFindings = append(report.SecretFindings, SecretSecurityFinding{
				Severity:    "MEDIUM",
				Type:        "OldCredentials",
				Resource:    fmt.Sprintf("Secret/%s", secret.Name),
				Description: "Credential secret is older than 90 days",
				Age:         age,
				LastRotated: secret.CreationTimestamp.Time,
				Remediation: "Rotate credentials regularly (recommended: every 90 days)",
			})
		}

		// Check for very old secrets (>1 year)
		if age > 365*24*time.Hour {
			report.SecretFindings = append(report.SecretFindings, SecretSecurityFinding{
				Severity:    SeverityHigh,
				Type:        "VeryOldCredentials",
				Resource:    fmt.Sprintf("Secret/%s", secret.Name),
				Description: "Credential secret is older than 1 year",
				Age:         age,
				LastRotated: secret.CreationTimestamp.Time,
				Remediation: "Rotate credentials immediately",
			})
		}

		// Check for missing required fields
		if _, hasUsername := secret.Data["username"]; !hasUsername {
			report.SecretFindings = append(report.SecretFindings, SecretSecurityFinding{
				Severity:    SeverityHigh,
				Type:        "MissingUsername",
				Resource:    fmt.Sprintf("Secret/%s", secret.Name),
				Description: "Credential secret missing username field",
				Age:         age,
				Remediation: "Ensure secret contains required 'username' field",
			})
		}

		if _, hasPassword := secret.Data["password"]; !hasPassword {
			report.SecretFindings = append(report.SecretFindings, SecretSecurityFinding{
				Severity:    SeverityHigh,
				Type:        "MissingPassword",
				Resource:    fmt.Sprintf("Secret/%s", secret.Name),
				Description: "Credential secret missing password field",
				Age:         age,
				Remediation: "Ensure secret contains required 'password' field",
			})
		}
	}

	return nil
}

// generateRecommendations generates security recommendations based on findings
func (m *SecurityMonitor) generateRecommendations(report *SecurityReport) {
	// TLS recommendations
	hasInsecureSkipVerify := false
	hasTLSErrors := false

	for _, finding := range report.TLSFindings {
		if finding.Type == "InsecureSkipVerify" {
			hasInsecureSkipVerify = true
		}
		if finding.Type == "TLSError" {
			hasTLSErrors = true
		}
	}

	if hasInsecureSkipVerify {
		report.Recommendations = append(report.Recommendations, SecurityRecommendation{
			Priority:    SeverityHigh,
			Title:       "Disable insecureSkipVerify",
			Description: "Multiple PhysicalHosts have TLS certificate verification disabled",
			Action:      "Configure proper TLS certificates or implement a custom CA bundle",
			Impact:      "Improves protection against man-in-the-middle attacks",
			Effort:      "Medium",
		})
	}

	if hasTLSErrors {
		report.Recommendations = append(report.Recommendations, SecurityRecommendation{
			Priority:    SeverityHigh,
			Title:       "Fix TLS Certificate Issues",
			Description: "TLS certificate validation errors found",
			Action:      "Review and fix certificate configuration for BMC endpoints",
			Impact:      "Ensures secure communication with hardware BMCs",
			Effort:      "Medium",
		})
	}

	// RBAC recommendations
	hasOverlyBroad := false
	for _, finding := range report.RBACFindings {
		if finding.OverlyBroad {
			hasOverlyBroad = true
			break
		}
	}

	if hasOverlyBroad {
		report.Recommendations = append(report.Recommendations, SecurityRecommendation{
			Priority:    SeverityCritical,
			Title:       "Reduce RBAC Permissions",
			Description: "Overly broad RBAC permissions detected",
			Action:      "Apply principle of least privilege to RBAC configurations",
			Impact:      "Significantly reduces attack surface and improves security posture",
			Effort:      "High",
		})
	}

	// Secret rotation recommendations
	hasOldSecrets := false
	for _, finding := range report.SecretFindings {
		if finding.Type == "OldCredentials" || finding.Type == "VeryOldCredentials" {
			hasOldSecrets = true
			break
		}
	}

	if hasOldSecrets {
		report.Recommendations = append(report.Recommendations, SecurityRecommendation{
			Priority:    "MEDIUM",
			Title:       "Implement Credential Rotation",
			Description: "Old credential secrets found that should be rotated",
			Action:      "Establish a regular credential rotation schedule (every 90 days)",
			Impact:      "Reduces risk from compromised credentials",
			Effort:      "Medium",
		})
	}
}

// calculateTotals calculates the total number of issues by severity
func (m *SecurityMonitor) calculateTotals(report *SecurityReport) {
	severityCounts := map[string]int{
		SeverityCritical: 0,
		SeverityHigh:     0,
		SeverityMedium:   0,
		SeverityLow:      0,
	}

	// Count TLS findings
	for _, finding := range report.TLSFindings {
		severityCounts[finding.Severity]++
	}

	// Count RBAC findings
	for _, finding := range report.RBACFindings {
		severityCounts[finding.Severity]++
	}

	// Count secret findings
	for _, finding := range report.SecretFindings {
		severityCounts[finding.Severity]++
	}

	// Count general findings
	for _, finding := range report.GeneralFindings {
		severityCounts[finding.Severity]++
	}

	report.CriticalIssues = severityCounts[SeverityCritical]
	report.HighIssues = severityCounts[SeverityHigh]
	report.MediumIssues = severityCounts[SeverityMedium]
	report.LowIssues = severityCounts[SeverityLow]
	report.TotalIssues = report.CriticalIssues + report.HighIssues + report.MediumIssues + report.LowIssues
}

// reportFindings reports security findings as Kubernetes events
func (m *SecurityMonitor) reportFindings(ctx context.Context, report *SecurityReport) {
	logger := log.FromContext(ctx).WithName("security-report")

	// Create events for critical and high severity issues
	for _, finding := range report.TLSFindings {
		if finding.Severity == SeverityCritical || finding.Severity == SeverityHigh {
			m.createSecurityEvent(ctx, "TLSSecurityIssue", finding.Severity, finding.Resource, finding.Description)
		}
	}

	for _, finding := range report.RBACFindings {
		if finding.Severity == SeverityCritical || finding.Severity == SeverityHigh {
			m.createSecurityEvent(ctx, "RBACSecurityIssue", finding.Severity, finding.Resource, finding.Description)
		}
	}

	for _, finding := range report.SecretFindings {
		if finding.Severity == SeverityCritical || finding.Severity == SeverityHigh {
			m.createSecurityEvent(ctx, "SecretSecurityIssue", finding.Severity, finding.Resource, finding.Description)
		}
	}

	// Log summary
	logger.Info("Security scan summary",
		"total_issues", report.TotalIssues,
		"critical", report.CriticalIssues,
		"high", report.HighIssues,
		"medium", report.MediumIssues,
		"low", report.LowIssues,
		"recommendations", len(report.Recommendations))
}

// createSecurityEvent creates a Kubernetes event for security findings
func (m *SecurityMonitor) createSecurityEvent(ctx context.Context, reason, severity, resource, message string) {
	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("security-%s-%d", strings.ToLower(reason), time.Now().Unix()),
			Namespace: m.Namespace,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:      "SecurityMonitor",
			Name:      "beskar7-security-monitor",
			Namespace: m.Namespace,
		},
		Reason:  reason,
		Message: fmt.Sprintf("[%s] %s: %s", severity, resource, message),
		Type:    "Warning",
		Source: corev1.EventSource{
			Component: "beskar7-security-monitor",
		},
		FirstTimestamp: metav1.NewTime(time.Now()),
		LastTimestamp:  metav1.NewTime(time.Now()),
		Count:          1,
	}

	if err := m.Create(ctx, event); err != nil {
		logger := log.FromContext(ctx).WithName("security-event")
		logger.Error(err, "Failed to create security event", "reason", reason, "resource", resource)
	}
}

// Helper functions

func (m *SecurityMonitor) isBeskar7Related(name string) bool {
	beskar7Patterns := []string{
		"beskar7", "manager-role", "controller", "infrastructure.cluster.x-k8s.io",
	}

	lowerName := strings.ToLower(name)
	for _, pattern := range beskar7Patterns {
		if strings.Contains(lowerName, pattern) {
			return true
		}
	}
	return false
}

func (m *SecurityMonitor) isCredentialSecret(secret *corev1.Secret) bool {
	// Check if secret has credential-like data
	if _, hasUsername := secret.Data["username"]; hasUsername {
		return true
	}
	if _, hasPassword := secret.Data["password"]; hasPassword {
		return true
	}

	// Check for credential-like names
	credentialPatterns := []string{
		"credential", "cred", "bmc", "redfish", "auth",
	}

	lowerName := strings.ToLower(secret.Name)
	for _, pattern := range credentialPatterns {
		if strings.Contains(lowerName, pattern) {
			return true
		}
	}

	return false
}

func (m *SecurityMonitor) extractPermissions(clusterRole *rbacv1.ClusterRole) []string {
	permissions := make([]string, 0)
	for _, rule := range clusterRole.Rules {
		for _, verb := range rule.Verbs {
			for _, resource := range rule.Resources {
				permissions = append(permissions, fmt.Sprintf("%s:%s", verb, resource))
			}
		}
	}
	return permissions
}
