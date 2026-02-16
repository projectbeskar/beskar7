//go:build integration

/*
Copyright 2024 The Beskar7 Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package emulation

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	// metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrastructurev1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
	"github.com/projectbeskar/beskar7/controllers"
	internalredfish "github.com/projectbeskar/beskar7/internal/redfish"
	"github.com/stmcginnis/gofish/redfish"
)

func TestHardwareEmulation(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Hardware Emulation Suite")
}

var _ = Describe("Hardware Emulation Tests", func() {
	var (
		mockServer *MockRedfishServer
		ctx        context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
	})

	AfterEach(func() {
		if mockServer != nil {
			mockServer.Close()
		}
	})

	Context("Vendor-Specific Hardware Emulation", func() {
		It("should emulate Dell PowerEdge server behavior", func() {
			// Create Dell server emulation
			mockServer = NewMockRedfishServer(VendorDell)
			mockServer.DisableAuth()
			defer mockServer.Close()

			// Test vendor-specific information from mock server
			srvInfo := mockServer.systemInfo
			Expect(srvInfo.Manufacturer).To(Equal("Dell Inc."))
			Expect(srvInfo.Model).To(ContainSubstring("PowerEdge"))

			// Test Dell-specific BIOS attributes
			biosAttrs := mockServer.biosAttributes
			Expect(biosAttrs).To(HaveKey("KernelArgs"))
			Expect(biosAttrs["BootMode"]).To(Equal("Uefi"))

			// Test Redfish client integration
			client := createRedfishClient(mockServer.GetURL(), "admin", "password123")
			rfInfo, err := client.GetSystemInfo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(rfInfo.Manufacturer).To(Equal("Dell Inc."))
		})

		It("should emulate HPE ProLiant server behavior", func() {
			mockServer = NewMockRedfishServer(VendorHPE)
			mockServer.DisableAuth()
			defer mockServer.Close()

			srvInfo := mockServer.systemInfo
			Expect(srvInfo.Manufacturer).To(Equal("HPE"))
			Expect(srvInfo.Model).To(ContainSubstring("ProLiant"))

			// Test HPE-specific BIOS attributes
			biosAttrs := mockServer.biosAttributes
			Expect(biosAttrs).To(HaveKey("UefiOptimizedBoot"))
			Expect(biosAttrs["BootOrderPolicy"]).To(Equal("AttemptOnce"))
		})

		It("should emulate Lenovo ThinkSystem server behavior", func() {
			mockServer = NewMockRedfishServer(VendorLenovo)
			mockServer.DisableAuth()
			defer mockServer.Close()

			srvInfo := mockServer.systemInfo
			Expect(srvInfo.Manufacturer).To(Equal("Lenovo"))
			Expect(srvInfo.Model).To(ContainSubstring("ThinkSystem"))

			// Test Lenovo-specific BIOS attributes
			biosAttrs := mockServer.biosAttributes
			Expect(biosAttrs).To(HaveKey("SystemBootSequence"))
			Expect(biosAttrs["SecureBootEnable"]).To(Equal("Enabled"))
		})

		It("should emulate Supermicro server behavior", func() {
			mockServer = NewMockRedfishServer(VendorSupermicro)
			mockServer.DisableAuth()
			defer mockServer.Close()

			srvInfo := mockServer.systemInfo
			Expect(srvInfo.Manufacturer).To(Equal("Supermicro"))
			Expect(srvInfo.Model).To(ContainSubstring("X12"))

			// Test Supermicro-specific BIOS attributes
			biosAttrs := mockServer.biosAttributes
			Expect(biosAttrs).To(HaveKey("BootFeature"))
			Expect(biosAttrs["QuietBoot"]).To(Equal("Enabled"))
		})
	})

	Context("Failure Scenario Testing", func() {
		BeforeEach(func() {
			mockServer = NewMockRedfishServer(VendorGeneric)
			mockServer.DisableAuth()
		})

		It("should handle network connectivity failures", func() {
			// Enable network error simulation
			mockServer.SetFailureMode(FailureConfig{
				NetworkErrors: true,
			})

			client := createRedfishClient(mockServer.GetURL(), "admin", "password123")
			_, err := client.GetSystemInfo(ctx)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("500"))
		})

		It("should handle authentication failures", func() {
			// Re-enable auth and force auth failures
			mockServer.authEnabled = true
			mockServer.SetFailureMode(FailureConfig{
				AuthFailures: true,
			})

			// Create client directly and assert creation error due to 401
			_, err := internalredfish.NewClientWithHTTPClient(
				ctx,
				mockServer.GetURL(),
				"admin",
				"password123",
				true,
				&http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}},
			)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("401"))
		})

		It("should handle slow response scenarios", func() {
			// Enable slow response simulation
			mockServer.SetFailureMode(FailureConfig{
				SlowResponses: true,
			})

			client := createRedfishClient(mockServer.GetURL(), "admin", "password123")

			start := time.Now()
			_, err := client.GetSystemInfo(ctx)
			duration := time.Since(start)

			Expect(err).NotTo(HaveOccurred())
			Expect(duration).To(BeNumerically(">=", 5*time.Second))
		})

		It("should handle power operation failures", func() {
			// Enable power failure simulation
			mockServer.SetFailureMode(FailureConfig{
				PowerFailures: true,
			})

			client := createRedfishClient(mockServer.GetURL(), "admin", "password123")
			err := client.SetPowerState(ctx, redfish.OnPowerState)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("500"))
		})
	})

	Context("PhysicalHost Controller Integration", func() {
		var (
			_ *controllers.PhysicalHostReconciler
			_ *infrastructurev1beta1.PhysicalHost
			_ *corev1.Secret
		)

		BeforeEach(func() {
			mockServer = NewMockRedfishServer(VendorDell)
			mockServer.DisableAuth()

			// Controller environment not used in these emulation tests
		})

		It("should successfully connect to emulated Dell server", func() {
			// This test would require a full test environment setup
			// For now, we test the Redfish client directly
			client := createRedfishClient(mockServer.GetURL(), "admin", "password123")

			systemInfo, err := client.GetSystemInfo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(systemInfo.Manufacturer).To(Equal("Dell Inc."))
			Expect(systemInfo.Model).To(Equal("PowerEdge R750"))
			Expect(systemInfo.SerialNumber).To(Equal("DELL123456789"))
		})

		It("should handle power state changes on emulated server", func() {
			client := createRedfishClient(mockServer.GetURL(), "admin", "password123")

			// Test power operations
			err := client.SetPowerState(ctx, redfish.OffPowerState)
			Expect(err).NotTo(HaveOccurred())

			// Verify power state changed
			ps, err := client.GetPowerState(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(ps)).To(Equal("Off"))
		})

		It("should handle boot source configuration on emulated server", func() {
			client := createRedfishClient(mockServer.GetURL(), "admin", "password123")

			// Test setting boot source
			err := client.SetBootSourceISO(ctx, "http://example.com/test.iso")
			Expect(err).NotTo(HaveOccurred())

			// Verify request was logged
			logs := mockServer.GetRequestLog()
			Expect(len(logs)).To(BeNumerically(">", 0))

			// Check for virtual media or boot override requests
			foundBootRequest := false
			for _, log := range logs {
				if log.URL == "/redfish/v1/Systems/1" && log.Method == "PATCH" {
					foundBootRequest = true
					break
				}
			}
			Expect(foundBootRequest).To(BeTrue())
		})
	})

	Context("Stress Testing and Concurrent Operations", func() {
		BeforeEach(func() {
			mockServer = NewMockRedfishServer(VendorGeneric)
			mockServer.DisableAuth()
		})

		It("should handle multiple concurrent connections", func() {
			numClients := 10
			done := make(chan bool, numClients)

			for i := 0; i < numClients; i++ {
				go func(clientID int) {
					defer GinkgoRecover()
					_ = clientID
					client := createRedfishClient(mockServer.GetURL(), "admin", "password123")

					// Each client performs several operations
					for j := 0; j < 5; j++ {
						_, err := client.GetSystemInfo(ctx)
						Expect(err).NotTo(HaveOccurred())

						err = client.SetPowerState(ctx, redfish.OnPowerState)
						Expect(err).NotTo(HaveOccurred())

						time.Sleep(10 * time.Millisecond) // Small delay between operations
					}
					done <- true
				}(i)
			}

			// Wait for all clients to complete
			for i := 0; i < numClients; i++ {
				Eventually(done).Should(Receive())
			}

			// Verify server handled all requests
			logs := mockServer.GetRequestLog()
			Expect(len(logs)).To(BeNumerically(">=", numClients*5*2)) // At least 2 requests per operation
		})

		It("should maintain state consistency under concurrent operations", func() {
			numOperations := 20
			done := make(chan bool, numOperations)

			for i := 0; i < numOperations; i++ {
				go func() {
					defer GinkgoRecover()
					client := createRedfishClient(mockServer.GetURL(), "admin", "password123")

					// Alternate between power states
					if i%2 == 0 {
						err := client.SetPowerState(ctx, redfish.OnPowerState)
						Expect(err).NotTo(HaveOccurred())
					} else {
						err := client.SetPowerState(ctx, redfish.OffPowerState)
						Expect(err).NotTo(HaveOccurred())
					}
					done <- true
				}()
			}

			// Wait for all operations to complete
			for i := 0; i < numOperations; i++ {
				Eventually(done).Should(Receive())
			}

			// Final state should be consistent
			client := createRedfishClient(mockServer.GetURL(), "admin", "password123")
			ps, err := client.GetPowerState(ctx)
			Expect(err).NotTo(HaveOccurred())
			// Power state should be either On or Off, not in an inconsistent state
			powerState := string(ps)
			Expect(powerState).To(SatisfyAny(Equal("On"), Equal("Off")))
		})
	})

	Context("Vendor-Specific Behavior Testing", func() {
		It("should test Dell BIOS attribute handling", func() {
			mockServer = NewMockRedfishServer(VendorDell)
			mockServer.DisableAuth()
			defer mockServer.Close()

			client := createRedfishClient(mockServer.GetURL(), "admin", "password123")

			// Test Dell-specific kernel args setting (vendor-specific behavior)
			// In a real implementation, this would test the vendor-specific boot parameter setting
			_, err := client.GetSystemInfo(ctx)
			Expect(err).NotTo(HaveOccurred())

			// This would test actual BIOS attribute setting in a full implementation
			// For now, we verify the server has the right vendor configuration
			Expect(mockServer.vendor).To(Equal(VendorDell))
			Expect(mockServer.biosAttributes).To(HaveKey("KernelArgs"))
		})

		It("should test HPE UEFI boot override behavior", func() {
			mockServer = NewMockRedfishServer(VendorHPE)
			mockServer.DisableAuth()
			defer mockServer.Close()

			client := createRedfishClient(mockServer.GetURL(), "admin", "password123")

			// Test HPE-specific UEFI target boot override
			err := client.SetBootSourceISO(ctx, "http://example.com/hpe-test.iso")
			Expect(err).NotTo(HaveOccurred())

			// Verify HPE-specific configuration
			Expect(mockServer.vendor).To(Equal(VendorHPE))
			Expect(mockServer.biosAttributes).To(HaveKey("UefiOptimizedBoot"))
		})
	})
})

// createRedfishClient creates a Redfish client with TLS verification disabled for testing
func createRedfishClient(address, username, password string) internalredfish.Client {
	// Create custom HTTP client that skips TLS verification
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	httpClient := &http.Client{Transport: transport}

	// Use the actual Redfish client factory with custom HTTP client
	client, err := internalredfish.NewClientWithHTTPClient(
		context.Background(),
		address,
		username,
		password,
		true, // insecure
		httpClient,
	)
	if err != nil {
		panic(fmt.Sprintf("Failed to create Redfish client: %v", err))
	}

	return client
}
