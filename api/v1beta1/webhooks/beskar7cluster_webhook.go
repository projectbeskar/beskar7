package webhooks

import (
	"context"
	"net"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
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

	// IPv4 / IPv6 literal — net.ParseIP handles both. Brackets-around-IPv6
	// (e.g. "[::1]") aren't accepted here because clusterv1.APIEndpoint.Host
	// is the host portion only; brackets are an authority-component construct.
	if ip := net.ParseIP(host); ip != nil {
		return allErrs
	}

	// Otherwise it must be a DNS-1123 subdomain (RFC 1123 with the
	// underscore-rejection clarification). validation.IsDNS1123Subdomain
	// returns a list of human-readable problems; concatenate them into a
	// single field.Error detail so the operator sees the actual reason
	// rather than a generic "must be a valid hostname".
	if errs := validation.IsDNS1123Subdomain(host); len(errs) > 0 {
		detail := "must be a valid IP address or DNS subdomain (RFC 1123): " + errs[0]
		allErrs = append(allErrs, field.Invalid(fieldPath, host, detail))
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

func (webhook *Beskar7ClusterWebhook) defaultBeskar7Cluster(cluster *infrav1beta1.Beskar7Cluster) error {
	// Set default port for control plane endpoint if host is specified but port is not
	if cluster.Spec.ControlPlaneEndpoint.Host != "" && cluster.Spec.ControlPlaneEndpoint.Port == 0 {
		cluster.Spec.ControlPlaneEndpoint.Port = 6443
	}

	return nil
}
