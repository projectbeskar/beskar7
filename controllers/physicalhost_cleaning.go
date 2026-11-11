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

package controllers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/stmcginnis/gofish/schemas"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	"github.com/projectbeskar/beskar7/internal/auth"
	internalmetrics "github.com/projectbeskar/beskar7/internal/metrics"
	internalredfish "github.com/projectbeskar/beskar7/internal/redfish"
)

// Cleaning run of a released host: released -> Cleaning -> Available, or -> Error (held).

const (
	// DefaultCleaningTimeout bounds a cleaning run from boot to report, below auth.CleaningTokenLifetime.
	DefaultCleaningTimeout = 20 * time.Minute

	// cleaningPollInterval is how often a booted cleaning run is looked at.
	cleaningPollInterval = 30 * time.Second

	// cleaningIdleRequeue is how often a host the controller cannot advance is looked at.
	cleaningIdleRequeue = 5 * time.Minute

	// cleanedRequestValue is the only CleanedRequestAnnotation value acted on.
	cleanedRequestValue = "cleaned"

	// cleanFailedReasonPrefix marks an Error message from the inspector's /clean-failed report.
	cleanFailedReasonPrefix = "inspector reported clean failure: "
)

// Event reasons of a cleaning run.
const (
	eventCleaningStarted  = "CleaningStarted"
	eventCleaningBooted   = "CleaningBooted"
	eventCleaningFinished = "CleaningFinished"
	eventCleaningFailed   = "CleaningFailed"
	eventCleaningSkipped  = "CleaningSkipped"
)

// cleaningEnabled reports whether the host's owner asked for a wipe on release.
func cleaningEnabled(host *infrav1.PhysicalHost) bool {
	return host.Spec.CleaningMode == infrav1.CleaningModeMetadata
}

// cleaningHeld reports whether an unclaimed host is held in Error by a failed cleaning run, read from HostCleaned so a BMC error cannot release it.
func cleaningHeld(host *infrav1.PhysicalHost) bool {
	if host.Spec.ConsumerRef != nil || host.Status.State != infrav1.StateError {
		return false
	}
	c := conditions.Get(host, infrav1.HostCleanedCondition)
	if c == nil || c.Status != metav1.ConditionFalse {
		return false
	}
	return c.Reason == infrav1.CleaningFailedReason || c.Reason == infrav1.CleaningTimedOutReason
}

// hostStateHeld reports whether a connection failure must leave the host's state alone.
func hostStateHeld(host *infrav1.PhysicalHost) bool {
	return inProvisioningSubState(host) || host.Status.State == infrav1.StateCleaning || cleaningHeld(host)
}

// releasedForCleaning reports whether an unclaimed host still carries a claim's state or HostClaimed marker and must be wiped first.
func releasedForCleaning(host *infrav1.PhysicalHost) bool {
	if host.Spec.ConsumerRef != nil || !cleaningEnabled(host) {
		return false
	}
	switch host.Status.State {
	case infrav1.StateInUse, infrav1.StateInspecting, infrav1.StateDeploying, infrav1.StateReady:
		return true
	}
	c := conditions.Get(host, infrav1.HostAvailableCondition)
	return c != nil && c.Status == metav1.ConditionFalse && c.Reason == infrav1.HostClaimedReason
}

// enterCleaning puts host at the start of a cleaning run.
func enterCleaning(host *infrav1.PhysicalHost) {
	host.Status.State = infrav1.StateCleaning
	host.Status.Ready = false
	host.Status.ErrorMessage = ""
	host.Status.CleaningTimestamp = nil
	setFalse(host, infrav1.HostCleanedCondition, infrav1.CleaningPendingReason, "Waiting to boot the cleaning run")
	setFalse(host, infrav1.HostAvailableCondition, infrav1.HostCleaningReason, "Wiping the disks before the host is Available again")
}

// startCleaningOnRelease moves a released host to Cleaning before the BMC is tried, so an outage cannot skip it.
func (r *PhysicalHostReconciler) startCleaningOnRelease(logger logr.Logger, host *infrav1.PhysicalHost) {
	if !releasedForCleaning(host) {
		return
	}
	logger.Info("Host released with cleaningMode Metadata; wiping its disks before it is Available", "previousState", host.Status.State)
	enterCleaning(host)
	r.event(host, corev1.EventTypeNormal, eventCleaningStarted, "Host released; cleaning its disks before it is Available")
}

// applyRetryCleaningAnnotation restarts the cleaning of a held host and always consumes the annotation.
func (r *PhysicalHostReconciler) applyRetryCleaningAnnotation(logger logr.Logger, host *infrav1.PhysicalHost) {
	if _, ok := host.Annotations[RetryCleaningAnnotation]; !ok {
		return
	}
	delete(host.Annotations, RetryCleaningAnnotation)
	if !cleaningHeld(host) || !cleaningEnabled(host) {
		logger.Info("Ignoring the retry-cleaning annotation: the host is not held after a failed cleaning run", "state", host.Status.State)
		return
	}
	logger.Info("Retrying the cleaning run as requested by annotation")
	enterCleaning(host)
	r.event(host, corev1.EventTypeNormal, eventCleaningStarted, "Cleaning retried by annotation")
}

// applyCleanedRequestAnnotation marks HostCleaned True for a bound /cleaned report on a booted run.
func (r *PhysicalHostReconciler) applyCleanedRequestAnnotation(ctx context.Context, logger logr.Logger, host *infrav1.PhysicalHost) {
	val, present := host.Annotations[CleanedRequestAnnotation]
	if !present {
		dropCallbackAnnotation(host, CleanedRequestAnnotation)
		return
	}
	if r.verifyCleaningReport(ctx, logger, host, CleanedRequestAnnotation) != callbackBound {
		return
	}
	dropCallbackAnnotation(host, CleanedRequestAnnotation)
	if val != cleanedRequestValue || host.Status.CleaningTimestamp == nil {
		logger.Info("Ignoring a cleaned report the host's cleaning run cannot have sent", "booted", host.Status.CleaningTimestamp != nil)
		return
	}
	logger.Info("Inspector reported the disks wiped")
	setTrue(host, infrav1.HostCleanedCondition, infrav1.HostCleanedReason)
}

// applyCleanFailedRequestAnnotation fails a booted run on a bound /clean-failed report.
func (r *PhysicalHostReconciler) applyCleanFailedRequestAnnotation(ctx context.Context, logger logr.Logger, host *infrav1.PhysicalHost) {
	val, present := host.Annotations[CleanFailedRequestAnnotation]
	if !present {
		dropCallbackAnnotation(host, CleanFailedRequestAnnotation)
		return
	}
	if r.verifyCleaningReport(ctx, logger, host, CleanFailedRequestAnnotation) != callbackBound {
		return
	}
	dropCallbackAnnotation(host, CleanFailedRequestAnnotation)
	if host.Status.CleaningTimestamp == nil || conditions.IsTrue(host, infrav1.HostCleanedCondition) {
		logger.Info("Ignoring a clean-failed report the host's cleaning run cannot have sent")
		return
	}
	if !strings.HasPrefix(val, cleanFailedReasonPrefix) {
		val = sanitizeReportedReason(val, cleanFailedReasonPrefix)
	}
	r.failCleaning(ctx, logger, host, cleaningFailure{
		Reason:  infrav1.CleaningFailedReason,
		Message: val,
		Outcome: internalmetrics.CleaningOutcomeFailed,
	})
}

// cleaningFailure describes how a cleaning run failed.
type cleaningFailure struct {
	Reason  string
	Message string
	Outcome internalmetrics.CleaningOutcome
}

// failCleaning holds host in Error and retires the run's credentials.
func (r *PhysicalHostReconciler) failCleaning(ctx context.Context, logger logr.Logger, host *infrav1.PhysicalHost, failure cleaningFailure) {
	logger.Info("Cleaning run failed; holding the host in Error", "reason", failure.Reason, "message", failure.Message)
	internalmetrics.RecordPhysicalHostCleaning(host.Namespace, failure.Outcome, cleaningElapsed(host))
	host.Status.CleaningTimestamp = nil
	r.updateStatus(host, infrav1.StateError, false, failure.Message)
	setFalse(host, infrav1.HostCleanedCondition, failure.Reason, "%s", failure.Message)
	setFalse(host, infrav1.HostAvailableCondition, failure.Reason,
		"The disks may still hold the previous claim's data; set the %s annotation to retry, or cleaningMode Disabled to release the host as it is", RetryCleaningAnnotation)
	if err := r.retireCleaningCredentials(ctx, logger, host); err != nil {
		logger.Error(err, "Failed to retire the cleaning run's credentials")
	}
	r.event(host, corev1.EventTypeWarning, eventCleaningFailed, failure.Message)
}

// cleaningElapsed is the time since the run booted, or zero before it has.
func cleaningElapsed(host *infrav1.PhysicalHost) time.Duration {
	if host.Status.CleaningTimestamp == nil {
		return 0
	}
	return time.Since(host.Status.CleaningTimestamp.Time)
}

// cleaningPass is one reconcile pass over a Cleaning host whose BMC answered.
type cleaningPass struct {
	Logger  logr.Logger
	Host    *infrav1.PhysicalHost
	Redfish internalredfish.Client
}

// reconcileCleaning advances a Cleaning host whose BMC answered.
func (r *PhysicalHostReconciler) reconcileCleaning(ctx context.Context, pass cleaningPass) (ctrl.Result, error) {
	host := pass.Host
	switch {
	case !cleaningEnabled(host):
		return r.skipCleaning(ctx, pass)
	case conditions.IsTrue(host, infrav1.HostCleanedCondition):
		return r.finishCleaning(ctx, pass)
	case host.Status.CleaningTimestamp != nil:
		return r.watchCleaningRun(ctx, pass)
	default:
		return r.bootCleaningRun(ctx, pass)
	}
}

// bootCleaningRun mints the run's credentials, then boots the inspector in clean mode.
func (r *PhysicalHostReconciler) bootCleaningRun(ctx context.Context, pass cleaningPass) (ctrl.Result, error) {
	host, logger := pass.Host, pass.Logger
	if r.CleaningImageURL == "" {
		setFalse(host, infrav1.HostCleanedCondition, infrav1.CleaningNotConfiguredReason,
			"The manager has no --cleaning-image-url to boot the cleaning run with")
		logger.Info("Cannot clean the host: the manager has no --cleaning-image-url")
		return ctrl.Result{RequeueAfter: cleaningIdleRequeue}, nil
	}

	creds, err := r.mintCleaningCredentials(ctx, host)
	if err != nil {
		if errors.Is(err, errBootstrapSecretNotOwned) {
			setFalse(host, infrav1.HostCleanedCondition, infrav1.CleaningPendingReason,
				"Secret %q is not owned by this host, so the cleaning credentials cannot be stored in it; delete it to let a fresh one be created",
				bootstrapTokenSecretName(host.Name))
		}
		return ctrl.Result{}, fmt.Errorf("mint the cleaning run's credentials: %w", err)
	}
	setBootstrapMirror(host, creds)

	if err := pass.Redfish.SetBootSourcePXE(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("set a one-time PXE boot for the cleaning run: %w", err)
	}
	if err := r.powerOnForCleaning(ctx, pass); err != nil {
		return ctrl.Result{}, err
	}

	now := metav1.Now()
	host.Status.CleaningTimestamp = &now
	setFalse(host, infrav1.HostCleanedCondition, infrav1.CleaningInProgressReason, "Booted the inspector in clean mode")
	logger.Info("Booted the cleaning run")
	r.event(host, corev1.EventTypeNormal, eventCleaningBooted, "Booted the inspector in clean mode")
	return ctrl.Result{RequeueAfter: cleaningPollInterval}, nil
}

// powerOnForCleaning powers the host on, or restarts it so the PXE override takes effect.
func (r *PhysicalHostReconciler) powerOnForCleaning(ctx context.Context, pass cleaningPass) error {
	host := pass.Host
	state, err := pass.Redfish.GetPowerState(ctx)
	if err != nil {
		return fmt.Errorf("read the power state before the cleaning boot: %w", err)
	}
	operation := internalmetrics.PowerOperationOn
	if state == schemas.OnPowerState {
		operation = internalmetrics.PowerOperationReset
		err = pass.Redfish.Reset(ctx)
	} else {
		err = pass.Redfish.SetPowerState(ctx, schemas.OnPowerState)
	}
	if err != nil {
		internalmetrics.RecordPhysicalHostPowerOperation(operation, host.Namespace, internalmetrics.ProvisioningOutcomeFailed)
		return fmt.Errorf("boot the host for the cleaning run: %w", err)
	}
	internalmetrics.RecordPhysicalHostPowerOperation(operation, host.Namespace, internalmetrics.ProvisioningOutcomeSuccess)
	return nil
}

// watchCleaningRun fails a booted run that has not reported within the cleaning timeout.
func (r *PhysicalHostReconciler) watchCleaningRun(ctx context.Context, pass cleaningPass) (ctrl.Result, error) {
	host := pass.Host
	elapsed := cleaningElapsed(host)
	timeout := r.cleaningTimeout()
	if elapsed < timeout {
		return ctrl.Result{RequeueAfter: min(cleaningPollInterval, timeout-elapsed)}, nil
	}
	if err := r.powerOffAfterCleaning(ctx, pass); err != nil {
		pass.Logger.Error(err, "Failed to power off the host after its cleaning run timed out")
	}
	r.failCleaning(ctx, pass.Logger, host, cleaningFailure{
		Reason:  infrav1.CleaningTimedOutReason,
		Message: fmt.Sprintf("Cleaning run did not report within %s", timeout),
		Outcome: internalmetrics.CleaningOutcomeTimedOut,
	})
	return ctrl.Result{RequeueAfter: cleaningIdleRequeue}, nil
}

// finishCleaning powers off a wiped host, retires its credentials and makes it Available.
func (r *PhysicalHostReconciler) finishCleaning(ctx context.Context, pass cleaningPass) (ctrl.Result, error) {
	host := pass.Host
	if err := r.powerOffAfterCleaning(ctx, pass); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.retireCleaningCredentials(ctx, pass.Logger, host); err != nil {
		return ctrl.Result{}, err
	}
	internalmetrics.RecordPhysicalHostCleaning(host.Namespace, internalmetrics.CleaningOutcomeCleaned, cleaningElapsed(host))
	host.Status.CleaningTimestamp = nil
	r.updateStatus(host, infrav1.StateAvailable, true, "")
	setTrue(host, infrav1.HostAvailableCondition, infrav1.HostAvailableReason)
	pass.Logger.Info("Disks wiped; host is Available")
	r.event(host, corev1.EventTypeNormal, eventCleaningFinished, "Disks wiped; host is Available")
	return ctrl.Result{RequeueAfter: cleaningIdleRequeue}, nil
}

// skipCleaning ends a run the owner disabled and makes the host Available without a wipe.
func (r *PhysicalHostReconciler) skipCleaning(ctx context.Context, pass cleaningPass) (ctrl.Result, error) {
	host := pass.Host
	if host.Status.CleaningTimestamp != nil {
		if err := r.powerOffAfterCleaning(ctx, pass); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.retireCleaningCredentials(ctx, pass.Logger, host); err != nil {
		return ctrl.Result{}, err
	}
	internalmetrics.RecordPhysicalHostCleaning(host.Namespace, internalmetrics.CleaningOutcomeSkipped, cleaningElapsed(host))
	releaseUncleaned(host)
	pass.Logger.Info("cleaningMode set to Disabled during the cleaning run; host is Available without a wipe")
	r.event(host, corev1.EventTypeWarning, eventCleaningSkipped, "cleaningMode set to Disabled; host is Available without a wipe")
	return ctrl.Result{RequeueAfter: cleaningIdleRequeue}, nil
}

// releaseUncleaned makes a host Available without a wipe and records that in HostCleaned.
func releaseUncleaned(host *infrav1.PhysicalHost) {
	host.Status.CleaningTimestamp = nil
	host.Status.State = infrav1.StateAvailable
	host.Status.Ready = true
	host.Status.ErrorMessage = ""
	setFalse(host, infrav1.HostCleanedCondition, infrav1.CleaningSkippedReason,
		"cleaningMode was set to Disabled; the disks were not wiped")
	setTrue(host, infrav1.HostAvailableCondition, infrav1.HostAvailableReason)
}

// powerOffAfterCleaning clears the boot override and forces the host off.
func (r *PhysicalHostReconciler) powerOffAfterCleaning(ctx context.Context, pass cleaningPass) error {
	host := pass.Host
	if err := pass.Redfish.ClearBootSourceOverride(ctx); err != nil {
		pass.Logger.Info("Failed to clear the boot source override after the cleaning run; continuing", "err", err.Error())
	}
	state, err := pass.Redfish.GetPowerState(ctx)
	if err == nil && state == schemas.OffPowerState {
		return nil
	}
	if err := pass.Redfish.ForcePowerOff(ctx); err != nil {
		internalmetrics.RecordPhysicalHostPowerOperation(internalmetrics.PowerOperationOff, host.Namespace, internalmetrics.ProvisioningOutcomeFailed)
		return fmt.Errorf("power off the host after its cleaning run: %w", err)
	}
	internalmetrics.RecordPhysicalHostPowerOperation(internalmetrics.PowerOperationOff, host.Namespace, internalmetrics.ProvisioningOutcomeSuccess)
	return nil
}

// mintCleaningCredentials replaces whatever the host's bootstrap-token Secret holds with fresh cleaning credentials.
func (r *PhysicalHostReconciler) mintCleaningCredentials(ctx context.Context, host *infrav1.PhysicalHost) (bootstrapCredentials, error) {
	existing, err := r.bootstrapTokenSecret(ctx, host)
	if err != nil {
		return bootstrapCredentials{}, err
	}
	token, _, err := auth.MintToken()
	if err != nil {
		return bootstrapCredentials{}, fmt.Errorf("mint cleaning token: %w", err)
	}
	nonce, _, err := auth.MintToken()
	if err != nil {
		return bootstrapCredentials{}, fmt.Errorf("mint cleaning boot nonce: %w", err)
	}
	now := time.Now()
	creds := bootstrapCredentials{
		token:          token,
		tokenIssuedAt:  now,
		tokenExpiresAt: now.Add(auth.CleaningTokenLifetime),
		nonce:          nonce,
		nonceExpiresAt: auth.NonceLifetimeFor(now).Time,
		purpose:        credentialPurposeClean,
		cleaningImage:  r.CleaningImageURL,
	}
	if err := writeBootstrapCredentialsSecret(ctx, writeCredentialsParams{
		Client:   r.Client,
		Scheme:   r.Scheme,
		Host:     host,
		Existing: existing,
		Creds:    creds,
	}); err != nil {
		return bootstrapCredentials{}, err
	}
	return creds, nil
}

// retireCleaningCredentials removes cleaning credentials, and their status mirror, from the host's own Secret.
func (r *PhysicalHostReconciler) retireCleaningCredentials(ctx context.Context, logger logr.Logger, host *infrav1.PhysicalHost) error {
	existing, err := r.bootstrapTokenSecret(ctx, host)
	if err != nil || existing == nil {
		return err
	}
	if !bootstrapSecretOwnedBy(existing, host) || !readBootstrapCredentials(existing).forCleaning() {
		return nil
	}
	if err := writeBootstrapCredentialsSecret(ctx, writeCredentialsParams{
		Client:   r.Client,
		Scheme:   r.Scheme,
		Host:     host,
		Existing: existing,
	}); err != nil {
		return fmt.Errorf("retire the cleaning credentials: %w", err)
	}
	setBootstrapMirror(host, bootstrapCredentials{})
	logger.V(1).Info("Retired the cleaning run's credentials")
	return nil
}

// bootstrapTokenSecret returns the host's bootstrap-token Secret, or nil when there is none.
func (r *PhysicalHostReconciler) bootstrapTokenSecret(ctx context.Context, host *infrav1.PhysicalHost) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: host.Namespace, Name: bootstrapTokenSecretName(host.Name)}
	if err := r.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get bootstrap-token Secret %s: %w", key.Name, err)
	}
	return secret, nil
}

// cleaningTimeout returns CleaningTimeout, or DefaultCleaningTimeout when unset.
func (r *PhysicalHostReconciler) cleaningTimeout() time.Duration {
	if r.CleaningTimeout > 0 {
		return r.CleaningTimeout
	}
	return DefaultCleaningTimeout
}

// validateCleaningConfig checks the cleaning flags at setup.
func (r *PhysicalHostReconciler) validateCleaningConfig() error {
	if r.cleaningTimeout() >= auth.CleaningTokenLifetime {
		return fmt.Errorf("PhysicalHostReconciler: cleaning timeout %s must be below the cleaning token lifetime %s",
			r.cleaningTimeout(), auth.CleaningTokenLifetime)
	}
	if r.CleaningImageURL == "" {
		return nil
	}
	return validateBootURL("--cleaning-image-url", r.CleaningImageURL)
}

// eventActionClean is the action of every cleaning event.
const eventActionClean = "Clean"

// verifyCleaningReport checks a cleaning report annotation against the host's cleaning credentials.
func (r *PhysicalHostReconciler) verifyCleaningReport(ctx context.Context, logger logr.Logger, host *infrav1.PhysicalHost, key string) callbackVerdict {
	creds, err := boundCleaningCredentials(ctx, r.bindingReader(), host)
	return checkCallbackAnnotation(callbackCheck{Logger: logger, Host: host, Key: key, Creds: creds, CredsErr: err})
}

// event records a cleaning event when the reconciler has a recorder.
func (r *PhysicalHostReconciler) event(host *infrav1.PhysicalHost, eventType, reason, message string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(host, nil, eventType, reason, eventActionClean, "%s", message)
}
