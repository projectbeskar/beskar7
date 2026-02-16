package webhooks

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"

	infrav1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
)

func TestBeskar7ClusterWebhook(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Beskar7Cluster Webhook Suite")
}

var _ = Describe("Beskar7Cluster Webhook", func() {
	var webhook *Beskar7ClusterWebhook
	var ctx context.Context

	BeforeEach(func() {
		webhook = &Beskar7ClusterWebhook{}
		ctx = context.Background()
	})

	Describe("ValidateCreate", func() {
		It("should accept valid Beskar7Cluster with IP address", func() {
			cluster := &infrav1beta1.Beskar7Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster",
					Namespace: "default",
				},
				Spec: infrav1beta1.Beskar7ClusterSpec{
					ControlPlaneEndpoint: clusterv1.APIEndpoint{
						Host: "192.168.1.100",
						Port: 6443,
					},
				},
			}

			warnings, err := webhook.ValidateCreate(ctx, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})

		It("should accept valid Beskar7Cluster with hostname", func() {
			cluster := &infrav1beta1.Beskar7Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster",
					Namespace: "default",
				},
				Spec: infrav1beta1.Beskar7ClusterSpec{
					ControlPlaneEndpoint: clusterv1.APIEndpoint{
						Host: "api.example.com",
						Port: 443,
					},
				},
			}

			warnings, err := webhook.ValidateCreate(ctx, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})

		It("should accept Beskar7Cluster without control plane endpoint", func() {
			cluster := &infrav1beta1.Beskar7Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster",
					Namespace: "default",
				},
				Spec: infrav1beta1.Beskar7ClusterSpec{
					// No ControlPlaneEndpoint specified
				},
			}

			warnings, err := webhook.ValidateCreate(ctx, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})

		It("should reject missing host when port is specified", func() {
			cluster := &infrav1beta1.Beskar7Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster",
					Namespace: "default",
				},
				Spec: infrav1beta1.Beskar7ClusterSpec{
					ControlPlaneEndpoint: clusterv1.APIEndpoint{
						Port: 6443,
						// Host missing
					},
				},
			}

			_, err := webhook.ValidateCreate(ctx, cluster)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("host is required when controlPlaneEndpoint is specified"))
		})

		It("should reject missing port when host is specified", func() {
			cluster := &infrav1beta1.Beskar7Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster",
					Namespace: "default",
				},
				Spec: infrav1beta1.Beskar7ClusterSpec{
					ControlPlaneEndpoint: clusterv1.APIEndpoint{
						Host: "api.example.com",
						// Port missing
					},
				},
			}

			_, err := webhook.ValidateCreate(ctx, cluster)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("port is required when controlPlaneEndpoint is specified"))
		})

		It("should reject invalid hostname", func() {
			cluster := &infrav1beta1.Beskar7Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster",
					Namespace: "default",
				},
				Spec: infrav1beta1.Beskar7ClusterSpec{
					ControlPlaneEndpoint: clusterv1.APIEndpoint{
						Host: "invalid..hostname",
						Port: 6443,
					},
				},
			}

			_, err := webhook.ValidateCreate(ctx, cluster)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("must be a valid IP address or hostname"))
		})

		It("should reject invalid port (too low)", func() {
			cluster := &infrav1beta1.Beskar7Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster",
					Namespace: "default",
				},
				Spec: infrav1beta1.Beskar7ClusterSpec{
					ControlPlaneEndpoint: clusterv1.APIEndpoint{
						Host: "api.example.com",
						Port: 0,
					},
				},
			}

			_, err := webhook.ValidateCreate(ctx, cluster)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("port is required when controlPlaneEndpoint is specified"))
		})

		It("should reject invalid port (too high)", func() {
			cluster := &infrav1beta1.Beskar7Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster",
					Namespace: "default",
				},
				Spec: infrav1beta1.Beskar7ClusterSpec{
					ControlPlaneEndpoint: clusterv1.APIEndpoint{
						Host: "api.example.com",
						Port: 65536,
					},
				},
			}

			_, err := webhook.ValidateCreate(ctx, cluster)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("port must be between 1 and 65535"))
		})

		It("should accept IPv6 addresses", func() {
			cluster := &infrav1beta1.Beskar7Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster",
					Namespace: "default",
				},
				Spec: infrav1beta1.Beskar7ClusterSpec{
					ControlPlaneEndpoint: clusterv1.APIEndpoint{
						Host: "2001:db8::1",
						Port: 6443,
					},
				},
			}

			warnings, err := webhook.ValidateCreate(ctx, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})

		It("should accept localhost", func() {
			cluster := &infrav1beta1.Beskar7Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster",
					Namespace: "default",
				},
				Spec: infrav1beta1.Beskar7ClusterSpec{
					ControlPlaneEndpoint: clusterv1.APIEndpoint{
						Host: "localhost",
						Port: 6443,
					},
				},
			}

			warnings, err := webhook.ValidateCreate(ctx, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})
	})

	Describe("Default", func() {
		It("should set default port when host is specified", func() {
			cluster := &infrav1beta1.Beskar7Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster",
					Namespace: "default",
				},
				Spec: infrav1beta1.Beskar7ClusterSpec{
					ControlPlaneEndpoint: clusterv1.APIEndpoint{
						Host: "api.example.com",
						// Port not set
					},
				},
			}

			err := webhook.Default(ctx, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(cluster.Spec.ControlPlaneEndpoint.Port).To(Equal(int32(6443)))
		})

		It("should not override existing port", func() {
			cluster := &infrav1beta1.Beskar7Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster",
					Namespace: "default",
				},
				Spec: infrav1beta1.Beskar7ClusterSpec{
					ControlPlaneEndpoint: clusterv1.APIEndpoint{
						Host: "api.example.com",
						Port: 443,
					},
				},
			}

			err := webhook.Default(ctx, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(cluster.Spec.ControlPlaneEndpoint.Port).To(Equal(int32(443)))
		})

		It("should not set port when host is empty", func() {
			cluster := &infrav1beta1.Beskar7Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster",
					Namespace: "default",
				},
				Spec: infrav1beta1.Beskar7ClusterSpec{
					// No ControlPlaneEndpoint specified
				},
			}

			err := webhook.Default(ctx, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(cluster.Spec.ControlPlaneEndpoint.Port).To(Equal(int32(0)))
		})
	})
})
