package coordination

import (
	"context"
	"fmt"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	infrastructurev1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
)

// ProvisioningQueue manages queued provisioning operations to prevent BMC overload
type ProvisioningQueue struct {
	mu                sync.RWMutex
	queue             []*ProvisioningRequest
	processing        map[string]*ProvisioningRequest // keyed by host name
	maxConcurrentOps  int
	maxQueueSize      int
	operationTimeout  time.Duration
	bmcCooldownPeriod time.Duration
	lastBMCOperation  map[string]time.Time // keyed by BMC address
	stopCh            chan struct{}
	workerWg          sync.WaitGroup
}

// ProvisioningRequest represents a queued provisioning operation
type ProvisioningRequest struct {
	ID          string
	Host        *infrastructurev1beta1.PhysicalHost
	Machine     *infrastructurev1beta1.Beskar7Machine
	Operation   ProvisioningOperation
	SubmittedAt time.Time
	StartedAt   *time.Time
	CompletedAt *time.Time
	Error       error
	ResultCh    chan *ProvisioningResult
	Context     context.Context
	CancelFunc  context.CancelFunc
}

// ProvisioningOperation defines the type of provisioning operation
type ProvisioningOperation string

const (
	OperationClaim       ProvisioningOperation = "claim"
	OperationProvision   ProvisioningOperation = "provision"
	OperationDeprovision ProvisioningOperation = "deprovision"
	OperationRelease     ProvisioningOperation = "release"
)

// ProvisioningResult represents the result of a provisioning operation
type ProvisioningResult struct {
	Success   bool
	Error     error
	Host      *infrastructurev1beta1.PhysicalHost
	Duration  time.Duration
	Retryable bool
}

// NewProvisioningQueue creates a new provisioning queue
func NewProvisioningQueue(maxConcurrent, maxQueueSize int) *ProvisioningQueue {
	return &ProvisioningQueue{
		queue:             make([]*ProvisioningRequest, 0),
		processing:        make(map[string]*ProvisioningRequest),
		maxConcurrentOps:  maxConcurrent,
		maxQueueSize:      maxQueueSize,
		operationTimeout:  5 * time.Minute,
		bmcCooldownPeriod: 10 * time.Second,
		lastBMCOperation:  make(map[string]time.Time),
		stopCh:            make(chan struct{}),
	}
}

// AcquireBMCPermit blocks until it is safe to perform BMC operations for the
// given host, enforcing global concurrency and per-BMC cooldown. It marks the
// host as processing so other acquires will wait. Call ReleaseBMCPermit when
// finished to free the slot and update cooldown.
func (pq *ProvisioningQueue) AcquireBMCPermit(ctx context.Context, host *infrastructurev1beta1.PhysicalHost) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	bmcAddress := host.Spec.RedfishConnection.Address

	for {
		// Fast path: check under lock
		pq.mu.Lock()
		// If already processing this host, allow re-entrant access
		if _, exists := pq.processing[host.Name]; exists {
			pq.mu.Unlock()
			return nil
		}

		// Check global concurrency
		if len(pq.processing) < pq.maxConcurrentOps {
			// Check BMC cooldown
			last, found := pq.lastBMCOperation[bmcAddress]
			if !found || time.Since(last) >= pq.bmcCooldownPeriod {
				// Reserve slot for this host
				pq.processing[host.Name] = &ProvisioningRequest{Host: host.DeepCopy()}
				pq.mu.Unlock()
				return nil
			}
		}
		pq.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			continue
		}
	}
}

// ReleaseBMCPermit releases the previously acquired permit and updates the
// BMC cooldown for the host's Redfish address.
func (pq *ProvisioningQueue) ReleaseBMCPermit(host *infrastructurev1beta1.PhysicalHost) {
	pq.mu.Lock()
	defer pq.mu.Unlock()
	delete(pq.processing, host.Name)
	bmcAddress := host.Spec.RedfishConnection.Address
	pq.lastBMCOperation[bmcAddress] = time.Now()
}

// Start starts the provisioning queue workers
func (pq *ProvisioningQueue) Start(ctx context.Context, numWorkers int) {
	logger := log.FromContext(ctx).WithValues("component", "ProvisioningQueue")
	logger.Info("Starting provisioning queue", "numWorkers", numWorkers, "maxConcurrent", pq.maxConcurrentOps)

	for i := 0; i < numWorkers; i++ {
		pq.workerWg.Add(1)
		go pq.worker(ctx, i)
	}
}

// Stop stops the provisioning queue
func (pq *ProvisioningQueue) Stop() {
	close(pq.stopCh)
	pq.workerWg.Wait()
}

// SubmitRequest submits a new provisioning request to the queue
func (pq *ProvisioningQueue) SubmitRequest(ctx context.Context, host *infrastructurev1beta1.PhysicalHost, machine *infrastructurev1beta1.Beskar7Machine, operation ProvisioningOperation) (*ProvisioningRequest, error) {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	// Check queue capacity
	if len(pq.queue) >= pq.maxQueueSize {
		return nil, fmt.Errorf("provisioning queue is full (max: %d)", pq.maxQueueSize)
	}

	// Check if host is already being processed
	if existing, found := pq.processing[host.Name]; found {
		return existing, fmt.Errorf("host %s already has operation %s in progress", host.Name, existing.Operation)
	}

	// Create request context with timeout
	reqCtx, cancel := context.WithTimeout(ctx, pq.operationTimeout)

	request := &ProvisioningRequest{
		ID:          fmt.Sprintf("%s-%s-%d", host.Name, operation, time.Now().UnixNano()),
		Host:        host.DeepCopy(),
		Machine:     machine.DeepCopy(),
		Operation:   operation,
		SubmittedAt: time.Now(),
		ResultCh:    make(chan *ProvisioningResult, 1),
		Context:     reqCtx,
		CancelFunc:  cancel,
	}

	// Add to queue
	pq.queue = append(pq.queue, request)

	logger := log.FromContext(ctx).WithValues(
		"requestID", request.ID,
		"host", host.Name,
		"operation", operation,
		"queueLength", len(pq.queue),
	)
	logger.Info("Submitted provisioning request to queue")

	return request, nil
}

// GetQueueStatus returns the current queue status
func (pq *ProvisioningQueue) GetQueueStatus() (queueLength int, processingCount int) {
	pq.mu.RLock()
	defer pq.mu.RUnlock()
	return len(pq.queue), len(pq.processing)
}

// worker processes requests from the queue
func (pq *ProvisioningQueue) worker(ctx context.Context, workerID int) {
	defer pq.workerWg.Done()

	logger := log.FromContext(ctx).WithValues("worker", workerID)
	logger.Info("Starting provisioning queue worker")

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("Worker context canceled")
			return
		case <-pq.stopCh:
			logger.Info("Worker stop signal received")
			return
		case <-ticker.C:
			if request := pq.getNextRequest(); request != nil {
				pq.processRequest(ctx, request, workerID)
			}
		}
	}
}

// getNextRequest gets the next request from the queue
func (pq *ProvisioningQueue) getNextRequest() *ProvisioningRequest {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	// Check if we've reached maximum concurrent operations
	if len(pq.processing) >= pq.maxConcurrentOps {
		return nil
	}

	// Find the next eligible request (considering BMC cooldown)
	for i, request := range pq.queue {
		bmcAddress := request.Host.Spec.RedfishConnection.Address
		if lastOp, found := pq.lastBMCOperation[bmcAddress]; found {
			if time.Since(lastOp) < pq.bmcCooldownPeriod {
				continue // Skip this request, BMC needs cooldown
			}
		}

		// Remove from queue and add to processing
		pq.queue = append(pq.queue[:i], pq.queue[i+1:]...)
		pq.processing[request.Host.Name] = request
		now := time.Now()
		request.StartedAt = &now

		return request
	}

	return nil
}

// processRequest processes a single provisioning request
func (pq *ProvisioningQueue) processRequest(ctx context.Context, request *ProvisioningRequest, workerID int) {
	logger := log.FromContext(ctx).WithValues(
		"worker", workerID,
		"requestID", request.ID,
		"host", request.Host.Name,
		"operation", request.Operation,
	)

	startTime := time.Now()
	logger.Info("Processing provisioning request")

	// Create result
	result := &ProvisioningResult{
		Success: false,
	}

	// Simulate processing based on operation type
	// In actual implementation, this would call the actual provisioning logic
	switch request.Operation {
	case OperationClaim:
		result = pq.processClaim(request)
	case OperationProvision:
		result = pq.processProvision(request)
	case OperationDeprovision:
		result = pq.processDeprovision(request)
	case OperationRelease:
		result = pq.processRelease(request)
	default:
		result.Error = fmt.Errorf("unknown operation: %s", request.Operation)
	}

	// Calculate duration
	result.Duration = time.Since(startTime)

	// Update BMC last operation time
	pq.mu.Lock()
	bmcAddress := request.Host.Spec.RedfishConnection.Address
	pq.lastBMCOperation[bmcAddress] = time.Now()
	delete(pq.processing, request.Host.Name)
	pq.mu.Unlock()

	// Mark as completed
	now := time.Now()
	request.CompletedAt = &now
	request.Error = result.Error

	// Send result
	select {
	case request.ResultCh <- result:
		logger.Info("Sent provisioning result", "success", result.Success, "duration", result.Duration)
	case <-request.Context.Done():
		logger.Info("Request context canceled before result could be sent")
	default:
		logger.Error(nil, "Failed to send result - channel blocked")
	}

	// Clean up
	request.CancelFunc()
}

// processClaim handles host claiming operations
func (pq *ProvisioningQueue) processClaim(request *ProvisioningRequest) *ProvisioningResult {
	// Simulate claim operation
	time.Sleep(100 * time.Millisecond)

	return &ProvisioningResult{
		Success:   true,
		Host:      request.Host,
		Retryable: false,
	}
}

// processProvision handles host provisioning operations
func (pq *ProvisioningQueue) processProvision(request *ProvisioningRequest) *ProvisioningResult {
	// Simulate provision operation (BMC calls)
	time.Sleep(2 * time.Second)

	return &ProvisioningResult{
		Success:   true,
		Host:      request.Host,
		Retryable: true,
	}
}

// processDeprovision handles host deprovisioning operations
func (pq *ProvisioningQueue) processDeprovision(request *ProvisioningRequest) *ProvisioningResult {
	// Simulate deprovision operation
	time.Sleep(1 * time.Second)

	return &ProvisioningResult{
		Success:   true,
		Host:      request.Host,
		Retryable: true,
	}
}

// processRelease handles host release operations
func (pq *ProvisioningQueue) processRelease(request *ProvisioningRequest) *ProvisioningResult {
	// Simulate release operation
	time.Sleep(50 * time.Millisecond)

	return &ProvisioningResult{
		Success:   true,
		Host:      request.Host,
		Retryable: false,
	}
}

// WaitForResult waits for the result of a provisioning request
func (pq *ProvisioningQueue) WaitForResult(request *ProvisioningRequest, timeout time.Duration) (*ProvisioningResult, error) {
	select {
	case result := <-request.ResultCh:
		return result, nil
	case <-request.Context.Done():
		return nil, request.Context.Err()
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout waiting for provisioning result after %v", timeout)
	}
}

// CancelRequest cancels a pending or processing request
func (pq *ProvisioningQueue) CancelRequest(requestID string) bool {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	// Check if it's in the queue
	for i, req := range pq.queue {
		if req.ID == requestID {
			req.CancelFunc()
			pq.queue = append(pq.queue[:i], pq.queue[i+1:]...)
			return true
		}
	}

	// Check if it's being processed
	for _, req := range pq.processing {
		if req.ID == requestID {
			req.CancelFunc()
			return true
		}
	}

	return false
}

// GetRequestStatus returns the status of a request
func (pq *ProvisioningQueue) GetRequestStatus(requestID string) (status string, found bool) {
	pq.mu.RLock()
	defer pq.mu.RUnlock()

	// Check queue
	for _, req := range pq.queue {
		if req.ID == requestID {
			return "queued", true
		}
	}

	// Check processing
	for _, req := range pq.processing {
		if req.ID == requestID {
			return "processing", true
		}
	}

	return "", false
}
