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
	"strings"
	"testing"
	"unicode/utf8"

	. "github.com/onsi/gomega"

	"sigs.k8s.io/cluster-api/controllers/clustercache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/beskar7/beskar7/api/v1beta2"
)

// boundedMessageMaxLen is far above a bounded message and far below a raw hostileWorkloadValue.
const boundedMessageMaxLen = 2048

// hostileWorkloadValue is what a tenant's cluster can send: multi-line, multibyte and ~20 KB long.
func hostileWorkloadValue() string {
	return "b7://evil/\nINFO forged log line " + strings.Repeat("ü", 10_000)
}

func expectBounded(g Gomega, s string) {
	g.Expect(len(s)).To(BeNumerically("<", boundedMessageMaxLen))
	g.Expect(s).NotTo(ContainSubstring("\n"))
	g.Expect(utf8.ValidString(s)).To(BeTrue())
}

func TestBoundWorkloadValue(t *testing.T) {
	atLimit := strings.Repeat("a", workloadValueMaxLen)
	cases := map[string]struct {
		in   string
		want string
	}{
		"short value is unchanged":    {in: foreignProviderID, want: foreignProviderID},
		"value at the limit is kept":  {in: atLimit, want: atLimit},
		"value over the limit is cut": {in: atLimit + "b", want: atLimit + workloadValueCutMarker},
		// "a" shifts every two-byte "ü" so the cut lands inside one; the half rune is dropped.
		"cut never splits a rune": {
			in:   "a" + strings.Repeat("ü", workloadValueMaxLen),
			want: "a" + strings.Repeat("ü", (workloadValueMaxLen-1)/2) + workloadValueCutMarker,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			got := boundWorkloadValue(tc.in)
			g.Expect(got).To(Equal(tc.want))
			g.Expect(utf8.ValidString(got)).To(BeTrue())
		})
	}
}

func TestReconcileNodeProviderID_MismatchBoundsNodeProviderID(t *testing.T) {
	g := NewWithT(t)
	f := newNodeProviderIDFixture(nodeProviderIDTestNamespace)
	workload := workloadWith(t, registeredNode(hostileWorkloadValue()))
	cache := clustercache.NewFakeClusterCache(workload, f.clusterKey())

	_, err := nodeProviderIDCase{fixture: f, workload: workload, cache: cache}.run(t)

	g.Expect(err).NotTo(HaveOccurred())
	cond := nodeProviderIDCondition(f.b7machine)
	g.Expect(cond.Reason).To(Equal(infrav1.NodeProviderIDMismatchReason))
	expectBounded(g, cond.Message)
}

func TestReconcileNodeProviderID_ReadErrorIsBounded(t *testing.T) {
	g := NewWithT(t)
	f := newNodeProviderIDFixture(nodeProviderIDTestNamespace)
	workload := interceptor.NewClient(workloadWith(t).(client.WithWatch), interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New(hostileWorkloadValue())
		},
	})
	cache := clustercache.NewFakeClusterCache(workload, f.clusterKey())

	_, err := nodeProviderIDCase{fixture: f, workload: workload, cache: cache}.run(t)

	g.Expect(err).NotTo(HaveOccurred())
	cond := nodeProviderIDCondition(f.b7machine)
	g.Expect(cond.Reason).To(Equal(infrav1.WorkloadClusterUnreachableReason))
	expectBounded(g, cond.Message)
}

func TestReconcileNodeProviderID_PatchErrorIsBounded(t *testing.T) {
	g := NewWithT(t)
	f := newNodeProviderIDFixture(nodeProviderIDTestNamespace)
	workload := interceptor.NewClient(workloadWith(t, registeredNode("")).(client.WithWatch), interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return errors.New(hostileWorkloadValue())
		},
	})
	cache := clustercache.NewFakeClusterCache(workload, f.clusterKey())

	_, err := nodeProviderIDCase{fixture: f, workload: workload, cache: cache}.run(t)

	g.Expect(err).To(HaveOccurred())
	expectBounded(g, err.Error())
}
