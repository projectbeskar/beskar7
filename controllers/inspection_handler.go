package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
)

// InspectionHandler handles HTTP requests from inspection images
type InspectionHandler struct {
	Client client.Client
	Log    logr.Logger
}

// InspectionReportRequest represents the JSON payload from inspection image
type InspectionReportRequest struct {
	// Namespace and name to identify the PhysicalHost
	Namespace string `json:"namespace"`
	HostName  string `json:"hostName"`

	// Hardware information from inspection
	Manufacturer string     `json:"manufacturer,omitempty"`
	Model        string     `json:"model,omitempty"`
	SerialNumber string     `json:"serialNumber,omitempty"`
	CPUs         []CPUData  `json:"cpus,omitempty"`
	Memory       []MemData  `json:"memory,omitempty"`
	Disks        []DiskData `json:"disks,omitempty"`
	NICs         []NICData  `json:"nics,omitempty"`

	// Additional metadata
	BootModeDetected string `json:"bootModeDetected,omitempty"`
	FirmwareVersion  string `json:"firmwareVersion,omitempty"`
}

type CPUData struct {
	ID        string `json:"id,omitempty"`
	Vendor    string `json:"vendor,omitempty"`
	Model     string `json:"model,omitempty"`
	Cores     int    `json:"cores,omitempty"`
	Threads   int    `json:"threads,omitempty"`
	Frequency string `json:"frequency,omitempty"`
}

type MemData struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Capacity string `json:"capacity,omitempty"`
	Speed    string `json:"speed,omitempty"`
}

type DiskData struct {
	Name         string `json:"name,omitempty"`
	Model        string `json:"model,omitempty"`
	SizeGB       int    `json:"sizeGB,omitempty"`
	Type         string `json:"type,omitempty"`
	SerialNumber string `json:"serialNumber,omitempty"`
}

type NICData struct {
	Name        string   `json:"name,omitempty"`
	MACAddress  string   `json:"macAddress,omitempty"`
	Driver      string   `json:"driver,omitempty"`
	Speed       string   `json:"speed,omitempty"`
	IPAddresses []string `json:"ipAddresses,omitempty"`
}

// ServeHTTP handles inspection report submissions
func (h *InspectionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log := h.Log.WithValues("method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)

	// Only accept POST requests
	if r.Method != http.MethodPost {
		log.Info("Method not allowed", "method", r.Method)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse request body
	var req InspectionReportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Error(err, "Failed to decode inspection report")
		http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	log = log.WithValues("namespace", req.Namespace, "host", req.HostName)
	log.Info("Received inspection report")

	// Validate required fields
	if req.Namespace == "" || req.HostName == "" {
		log.Info("Missing required fields")
		http.Error(w, "namespace and hostName are required", http.StatusBadRequest)
		return
	}

	// Update PhysicalHost with inspection report
	ctx := context.Background()
	if err := h.updatePhysicalHost(ctx, req); err != nil {
		log.Error(err, "Failed to update PhysicalHost")
		http.Error(w, fmt.Sprintf("Failed to update PhysicalHost: %v", err), http.StatusInternalServerError)
		return
	}

	log.Info("Successfully updated PhysicalHost with inspection report")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(map[string]string{
		"status":  "success",
		"message": "Inspection report received and processed",
	}); err != nil {
		log.Error(err, "Failed to encode response")
	}
}

// updatePhysicalHost updates the PhysicalHost with inspection report data
func (h *InspectionHandler) updatePhysicalHost(ctx context.Context, req InspectionReportRequest) error {
	// Get PhysicalHost
	physicalHost := &infrastructurev1beta1.PhysicalHost{}
	key := types.NamespacedName{
		Namespace: req.Namespace,
		Name:      req.HostName,
	}

	if err := h.Client.Get(ctx, key, physicalHost); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("PhysicalHost %s/%s not found", req.Namespace, req.HostName)
		}
		return fmt.Errorf("failed to get PhysicalHost: %w", err)
	}

	// Convert request data to InspectionReport
	report := &infrastructurev1beta1.InspectionReport{
		Timestamp:        metav1.Now(),
		Manufacturer:     req.Manufacturer,
		Model:            req.Model,
		SerialNumber:     req.SerialNumber,
		BootModeDetected: req.BootModeDetected,
		FirmwareVersion:  req.FirmwareVersion,
	}

	// Convert CPUs
	for _, cpu := range req.CPUs {
		report.CPUs = append(report.CPUs, infrastructurev1beta1.CPUInfo{
			ID:        cpu.ID,
			Vendor:    cpu.Vendor,
			Model:     cpu.Model,
			Cores:     cpu.Cores,
			Threads:   cpu.Threads,
			Frequency: cpu.Frequency,
		})
	}

	// Convert Memory
	for _, mem := range req.Memory {
		report.Memory = append(report.Memory, infrastructurev1beta1.MemoryInfo{
			ID:       mem.ID,
			Type:     mem.Type,
			Capacity: mem.Capacity,
			Speed:    mem.Speed,
		})
	}

	// Convert Disks
	for _, disk := range req.Disks {
		report.Disks = append(report.Disks, infrastructurev1beta1.DiskInfo{
			Name:         disk.Name,
			Model:        disk.Model,
			SizeGB:       disk.SizeGB,
			Type:         disk.Type,
			SerialNumber: disk.SerialNumber,
		})
	}

	// Convert NICs
	for _, nic := range req.NICs {
		report.NICs = append(report.NICs, infrastructurev1beta1.NICInfo{
			Name:        nic.Name,
			MACAddress:  nic.MACAddress,
			Driver:      nic.Driver,
			Speed:       nic.Speed,
			IPAddresses: nic.IPAddresses,
		})
	}

	// Update PhysicalHost status
	physicalHost.Status.InspectionReport = report
	physicalHost.Status.InspectionPhase = infrastructurev1beta1.InspectionComplete

	if err := h.Client.Status().Update(ctx, physicalHost); err != nil {
		return fmt.Errorf("failed to update PhysicalHost status: %w", err)
	}

	return nil
}

// SetupInspectionServer sets up the HTTP server for inspection reports
func SetupInspectionServer(mgr ctrl.Manager, port int) error {
	handler := &InspectionHandler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("inspection-handler"),
	}

	mux := http.NewServeMux()
	mux.Handle("/api/v1/inspection", handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("ok")); err != nil {
			ctrl.Log.WithName("inspection-server").Error(err, "Failed to write health check response")
		}
	})

	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", port),
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start server in goroutine
	go func() {
		ctrl.Log.WithName("inspection-server").Info("Starting inspection HTTP server", "port", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			ctrl.Log.WithName("inspection-server").Error(err, "Failed to start inspection HTTP server")
		}
	}()

	// Register shutdown with manager
	if err := mgr.Add(&inspectionServerRunnable{server: server}); err != nil {
		return fmt.Errorf("failed to add inspection server to manager: %w", err)
	}

	return nil
}

// inspectionServerRunnable implements manager.Runnable for graceful shutdown
type inspectionServerRunnable struct {
	server *http.Server
}

func (r *inspectionServerRunnable) Start(ctx context.Context) error {
	<-ctx.Done()
	ctrl.Log.WithName("inspection-server").Info("Shutting down inspection HTTP server")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return r.server.Shutdown(shutdownCtx)
}
