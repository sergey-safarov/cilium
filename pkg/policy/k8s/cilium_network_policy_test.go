// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package k8s

import (
	"testing"

	"github.com/cilium/hive/hivetest"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cilium_v2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	"github.com/cilium/cilium/pkg/k8s/resource"
	k8sSynced "github.com/cilium/cilium/pkg/k8s/synced"
	"github.com/cilium/cilium/pkg/k8s/types"
	"github.com/cilium/cilium/pkg/loadbalancer"
	"github.com/cilium/cilium/pkg/option"
	"github.com/cilium/cilium/pkg/policy/api"
	policytypes "github.com/cilium/cilium/pkg/policy/types"
)

func Test_GH33432(t *testing.T) {
	policyAdd := make(chan api.Rules, 1)
	policyImporter := &fakePolicyImporter{
		OnUpdatePolicy: func(upd *policytypes.PolicyUpdate) {
			policyAdd <- upd.Rules
		},
	}

	cnp := &types.SlimCNP{
		CiliumNetworkPolicy: &cilium_v2.CiliumNetworkPolicy{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "cilium.io/v2",
				Kind:       "CiliumNetworkPolicy",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cnp-gh-33432",
				Namespace: "test",
			},
			Spec: &api.Rule{
				EndpointSelector: api.NewESFromLabels(),
				Egress: []api.EgressRule{
					{
						EgressCommonRule: api.EgressCommonRule{
							ToCIDR:      []api.CIDR{"1.1.1.1/32"},
							ToEndpoints: nil, // initially ToEndpoints is a nil slice
						},
						ToPorts: []api.PortRule{{
							Ports: []api.PortProtocol{
								{Port: "80", Protocol: api.ProtoTCP},
							},
						}},
					},
				},
			},
		},
	}
	cnpKey := resource.NewKey(cnp)
	cnpResourceID := resourceIDForCiliumNetworkPolicy(cnpKey, cnp)

	p := &policyWatcher{
		log:                hivetest.Logger(t),
		config:             &option.DaemonConfig{},
		k8sResourceSynced:  &k8sSynced.Resources{CacheStatus: make(k8sSynced.CacheStatus)},
		k8sAPIGroups:       &k8sSynced.APIGroups{},
		policyImporter:     policyImporter,
		cnpCache:           map[resource.Key]*types.SlimCNP{},
		toServicesPolicies: map[resource.Key]struct{}{},
		cnpByServiceID:     map[loadbalancer.ServiceName]map[resource.Key]struct{}{},
		metricsManager:     NewCNPMetricsNoop(),
	}

	err := p.onUpsert(cnp, cnpKey, k8sAPIGroupCiliumNetworkPolicyV2, cnpResourceID, nil)
	assert.NoError(t, err)

	// added rules should have a nil ToEndpoints slice
	rules := <-policyAdd
	assert.Len(t, rules, 1)
	assert.Len(t, rules[0].Egress, 1)
	assert.Equal(t, api.CIDRSlice{"1.1.1.1/32"}, rules[0].Egress[0].EgressCommonRule.ToCIDR)
	assert.Len(t, rules[0].Egress[0].ToPorts, 1)
	assert.Len(t, rules[0].Egress[0].ToPorts[0].Ports, 1)
	assert.Equal(t, []api.PortProtocol{{Port: "80", Protocol: api.ProtoTCP}}, rules[0].Egress[0].ToPorts[0].Ports)
	assert.Nil(t, rules[0].Egress[0].EgressCommonRule.ToEndpoints)

	updCNP := cnp.DeepCopy()
	updCNP.Generation++

	// update ToEndpoints with an empty non-nil slice
	updCNP.Spec.Egress[0].ToEndpoints = []api.EndpointSelector{}

	updCNPKey := resource.NewKey(updCNP)
	updCNPResourceID := resourceIDForCiliumNetworkPolicy(updCNPKey, updCNP)

	err = p.onUpsert(updCNP, updCNPKey, k8sAPIGroupCiliumNetworkPolicyV2, updCNPResourceID, nil)
	assert.NoError(t, err)

	// policy update should be propagated and the new rules should be the same
	// except for the empty non-nil ToEndpoints slice
	rules = <-policyAdd
	assert.Len(t, rules, 1)
	assert.Len(t, rules[0].Egress, 1)
	assert.Equal(t, api.CIDRSlice{"1.1.1.1/32"}, rules[0].Egress[0].EgressCommonRule.ToCIDR)
	assert.Len(t, rules[0].Egress[0].ToPorts, 1)
	assert.Len(t, rules[0].Egress[0].ToPorts[0].Ports, 1)
	assert.Equal(t, []api.PortProtocol{{Port: "80", Protocol: api.ProtoTCP}}, rules[0].Egress[0].ToPorts[0].Ports)
	assert.NotNil(t, rules[0].Egress[0].EgressCommonRule.ToEndpoints)
	assert.Empty(t, rules[0].Egress[0].EgressCommonRule.ToEndpoints)
}
