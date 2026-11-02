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

	"github.com/go-logr/logr"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

// reconcileNetworkData publishes NetworkDataReady for the host's network data
// Secret, applying the same checks as the /networkdata handler. It needs
// nothing from the BMC, so it runs before the BMC is tried.
func (r *PhysicalHostReconciler) reconcileNetworkData(ctx context.Context, logger logr.Logger, host *infrav1.PhysicalHost) {
	if host.Spec.NetworkDataSecretRef == "" {
		conditions.Delete(host, infrav1.NetworkDataReadyCondition)
		return
	}
	_, err := loadNetworkData(ctx, r.Client, host)
	if err == nil {
		setTrue(host, infrav1.NetworkDataReadyCondition, infrav1.NetworkDataAvailableReason)
		return
	}
	reason := networkDataReason(err)
	if reason == "" {
		logger.Error(err, "Network data Secret could not be read, NetworkDataReady left as it is")
		return
	}
	setFalse(host, infrav1.NetworkDataReadyCondition, reason, "%s", err.Error())
}
