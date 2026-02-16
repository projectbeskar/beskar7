package webhooks

import (
	"context"
	"net"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	infrav1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
)

// Beskar7ClusterWebhook implements a validating and defaulting webhook for Beskar7Cluster.
type Beskar7ClusterWebhook struct{}

// SetupWebhookWithManager sets up the webhook with the manager.
func (webhook *Beskar7ClusterWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&infrav1beta1.Beskar7Cluster{}).
		WithValidator(webhook).
		WithDefaulter(webhook).
		Complete()
}

// +kubebuilder:webhook:verbs=create;update,path=/validate-infrastructure-cluster-x-k8s-io-v1beta1-beskar7cluster,mutating=false,failurePolicy=fail,matchPolicy=Equivalent,groups=infrastructure.cluster.x-k8s.io,resources=beskar7clusters,versions=v1beta1,name=validation.beskar7cluster.infrastructure.cluster.x-k8s.io,sideEffects=None,admissionReviewVersions=v1
// +kubebuilder:webhook:verbs=create;update,path=/mutate-infrastructure-cluster-x-k8s-io-v1beta1-beskar7cluster,mutating=true,failurePolicy=fail,matchPolicy=Equivalent,groups=infrastructure.cluster.x-k8s.io,resources=beskar7clusters,versions=v1beta1,name=defaulting.beskar7cluster.infrastructure.cluster.x-k8s.io,sideEffects=None,admissionReviewVersions=v1

var _ webhook.CustomValidator = &Beskar7ClusterWebhook{}
var _ webhook.CustomDefaulter = &Beskar7ClusterWebhook{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type.
func (webhook *Beskar7ClusterWebhook) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	cluster := obj.(*infrav1beta1.Beskar7Cluster)
	warnings, err := webhook.validateBeskar7Cluster(cluster)
	if err != nil {
		return warnings, err
	}

	return warnings, nil
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type.
func (webhook *Beskar7ClusterWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	newCluster := newObj.(*infrav1beta1.Beskar7Cluster)

	warnings, err := webhook.validateBeskar7Cluster(newCluster)
	if err != nil {
		return warnings, err
	}

	return warnings, nil
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type.
func (webhook *Beskar7ClusterWebhook) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	// No specific validations needed for deletion
	return nil, nil
}

// Default implements webhook.CustomDefaulter so a webhook will be registered for the type.
func (webhook *Beskar7ClusterWebhook) Default(ctx context.Context, obj runtime.Object) error {
	cluster := obj.(*infrav1beta1.Beskar7Cluster)
	return webhook.defaultBeskar7Cluster(cluster)
}

func (webhook *Beskar7ClusterWebhook) validateBeskar7Cluster(cluster *infrav1beta1.Beskar7Cluster) (admission.Warnings, error) {
	var allErrs field.ErrorList
	var warnings admission.Warnings

	// Validate ControlPlaneEndpoint if set
	if cluster.Spec.ControlPlaneEndpoint.Host != "" || cluster.Spec.ControlPlaneEndpoint.Port != 0 {
		if errs := webhook.validateControlPlaneEndpoint(cluster.Spec.ControlPlaneEndpoint); len(errs) > 0 {
			allErrs = append(allErrs, errs...)
		}
	}

	if len(allErrs) > 0 {
		return warnings, apierrors.NewInvalid(
			cluster.GroupVersionKind().GroupKind(),
			cluster.Name,
			allErrs,
		)
	}

	return warnings, nil
}

func (webhook *Beskar7ClusterWebhook) validateControlPlaneEndpoint(endpoint clusterv1.APIEndpoint) field.ErrorList {
	var allErrs field.ErrorList
	fieldPath := field.NewPath("spec", "controlPlaneEndpoint")

	// Validate host
	if endpoint.Host == "" {
		allErrs = append(allErrs, field.Required(
			fieldPath.Child("host"),
			"host is required when controlPlaneEndpoint is specified",
		))
	} else {
		if errs := webhook.validateHost(endpoint.Host, fieldPath.Child("host")); len(errs) > 0 {
			allErrs = append(allErrs, errs...)
		}
	}

	// Validate port - check range first, then required
	if endpoint.Port != 0 {
		if errs := webhook.validatePort(endpoint.Port, fieldPath.Child("port")); len(errs) > 0 {
			allErrs = append(allErrs, errs...)
		}
	} else {
		allErrs = append(allErrs, field.Required(
			fieldPath.Child("port"),
			"port is required when controlPlaneEndpoint is specified",
		))
	}

	return allErrs
}

func (webhook *Beskar7ClusterWebhook) validateHost(host string, fieldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	// Check if it's a valid IP address
	if ip := net.ParseIP(host); ip != nil {
		// Valid IP address
		return allErrs
	}

	// Check if it's a valid hostname/FQDN
	if !webhook.isValidHostname(host) {
		allErrs = append(allErrs, field.Invalid(
			fieldPath,
			host,
			"must be a valid IP address or hostname",
		))
	}

	return allErrs
}

func (webhook *Beskar7ClusterWebhook) validatePort(port int32, fieldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	// Validate port range
	if port < 1 || port > 65535 {
		allErrs = append(allErrs, field.Invalid(
			fieldPath,
			port,
			"port must be between 1 and 65535",
		))
	}

	// Note: Well-known ports (< 1024) other than 443 and 6443 might require special privileges
	// but are not considered validation errors

	return allErrs
}

func (webhook *Beskar7ClusterWebhook) isValidHostname(hostname string) bool {
	// Basic hostname validation
	if hostname == "" || len(hostname) > 253 {
		return false
	}

	// Check for invalid characters like consecutive dots
	if strings.Contains(hostname, "..") {
		return false
	}

	// Check each label in the hostname
	labels := strings.Split(hostname, ".")
	for _, label := range labels {
		if !webhook.isValidHostnameLabel(label) {
			return false
		}
	}

	return true
}

// isValidHostnameLabel validates a single hostname label
func (webhook *Beskar7ClusterWebhook) isValidHostnameLabel(label string) bool {
	if label == "" || len(label) > 63 {
		return false
	}
	// Check for valid characters (alphanumeric and hyphens, but not starting/ending with hyphen)
	for i, r := range label {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && (r != '-' || i == 0 || i == len(label)-1) {
			return false
		}
	}
	return true
}

func (webhook *Beskar7ClusterWebhook) defaultBeskar7Cluster(cluster *infrav1beta1.Beskar7Cluster) error {
	// Set default port for control plane endpoint if host is specified but port is not
	if cluster.Spec.ControlPlaneEndpoint.Host != "" && cluster.Spec.ControlPlaneEndpoint.Port == 0 {
		cluster.Spec.ControlPlaneEndpoint.Port = 6443
	}

	return nil
}
