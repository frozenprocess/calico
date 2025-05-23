// Copyright (c) 2021-2025 Tigera, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.package util

package integration

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	v3 "github.com/projectcalico/api/pkg/apis/projectcalico/v3"
	calicoclient "github.com/projectcalico/api/pkg/client/clientset_generated/clientset"
	"github.com/projectcalico/api/pkg/lib/numorstring"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/projectcalico/calico/libcalico-go/lib/apiconfig"
	libapiv3 "github.com/projectcalico/calico/libcalico-go/lib/apis/v3"
	libclient "github.com/projectcalico/calico/libcalico-go/lib/clientv3"
	"github.com/projectcalico/calico/libcalico-go/lib/options"
)

// TestGroupVersion is trivial.
func TestGroupVersion(t *testing.T) {
	rootTestFunc := func() func(t *testing.T) {
		return func(t *testing.T) {
			client, shutdownServer := getFreshApiserverAndClient(t, func() runtime.Object {
				return &v3.NetworkPolicy{}
			})
			defer shutdownServer()
			if err := testGroupVersion(client); err != nil {
				t.Fatal(err)
			}
		}
	}

	if !t.Run("group version", rootTestFunc()) {
		t.Error("test failed")
	}
}

func testGroupVersion(client calicoclient.Interface) error {
	gv := client.ProjectcalicoV3().RESTClient().APIVersion()
	if gv.Group != v3.GroupName {
		return fmt.Errorf("we should be testing the servicecatalog group, not %s", gv.Group)
	}
	return nil
}

func TestEtcdHealthCheckerSuccess(t *testing.T) {
	serverConfig := NewTestServerConfig()
	_, _, clientconfig, shutdownServer := withConfigGetFreshApiserverServerAndClient(t, serverConfig)
	t.Log(clientconfig.Host)
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	c := &http.Client{Transport: tr}
	var success bool
	var resp *http.Response
	var err error
	retryInterval := 500 * time.Millisecond
	for i := 0; i < 5; i++ {
		resp, err = c.Get(clientconfig.Host + "/healthz")
		if err != nil || http.StatusOK != resp.StatusCode {
			success = false
			time.Sleep(retryInterval)
		} else {
			success = true
			break
		}
	}

	if !success {
		t.Fatal("health check endpoint should not have failed")
	}

	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal("couldn't read response body", err)
	}
	if strings.Contains(string(body), "healthz check failed") {
		t.Fatal("health check endpoint should not have failed")
	}

	defer shutdownServer()
}

// TestNoName checks that all creates fail for objects that have no
// name given.
func TestNoName(t *testing.T) {
	rootTestFunc := func() func(t *testing.T) {
		return func(t *testing.T) {
			client, shutdownServer := getFreshApiserverAndClient(t, func() runtime.Object {
				return &v3.NetworkPolicy{}
			})
			defer shutdownServer()
			if err := testNoName(client); err != nil {
				t.Fatal(err)
			}
		}
	}

	if !t.Run("no-name", rootTestFunc()) {
		t.Errorf("NoName test failed")
	}
}

func testNoName(client calicoclient.Interface) error {
	cClient := client.ProjectcalicoV3()

	ns := "default"

	if p, e := cClient.NetworkPolicies(ns).Create(context.Background(), &v3.NetworkPolicy{}, metav1.CreateOptions{}); nil == e {
		return fmt.Errorf("needs a name (%s)", p.Name)
	}

	return nil
}

// TestNetworkPolicyClient exercises the NetworkPolicy client.
func TestNetworkPolicyClient(t *testing.T) {
	const name = "test-networkpolicy"
	rootTestFunc := func() func(t *testing.T) {
		return func(t *testing.T) {
			client, shutdownServer := getFreshApiserverAndClient(t, func() runtime.Object {
				return &v3.NetworkPolicy{}
			})
			defer shutdownServer()
			if err := testNetworkPolicyClient(client, name); err != nil {
				t.Fatal(err)
			}
		}
	}

	if !t.Run(name, rootTestFunc()) {
		t.Errorf("test-networkpolicy test failed")
	}
}

func testNetworkPolicyClient(client calicoclient.Interface, name string) error {
	ns := "default"
	defaultTierPolicyName := "default" + "." + name
	policyClient := client.ProjectcalicoV3().NetworkPolicies(ns)
	policy := &v3.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: defaultTierPolicyName}}
	ctx := context.Background()

	// start from scratch
	policies, err := policyClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing policies (%s)", err)
	}
	if policies.Items == nil {
		return fmt.Errorf("Items field should not be set to nil")
	}
	if len(policies.Items) > 0 {
		return fmt.Errorf("policies should not exist on start, had %v policies", len(policies.Items))
	}

	// Create a policy without the "default" prefix. It should be returned back without the prefix.
	policy2 := &v3.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name}}
	policyServer, err := policyClient.Create(ctx, policy2, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating the policy '%v' (%v)", policy2, err)
	}
	if name != policyServer.Name {
		return fmt.Errorf("policy name prefix was defaulted by the apiserver on create: %v", policyServer)
	}

	// Update that policy. We should be able to use the same name that we used to create it (i.e., without the "default" prefix).
	policyServer.Name = name
	policyServer.Labels = map[string]string{"foo": "bar"}
	policyServer, err = policyClient.Update(ctx, policyServer, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("error updating the policy '%v' (%v)", policyServer, err)
	}
	if defaultTierPolicyName == policyServer.Name {
		return fmt.Errorf("policy name prefix was defaulted by the apiserver on update: %v", policyServer)
	}

	// Delete that policy. We should be able to use the same name that we used to create it (i.e., without the "default" prefix).
	err = policyClient.Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("error deleting the policy '%v' (%v)", name, err)
	}

	// Now create a policy with the "default" prefix. It should be created as-is.
	policyServer, err = policyClient.Create(ctx, policy, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating the policy '%v' (%v)", policy, err)
	}
	if defaultTierPolicyName != policyServer.Name {
		return fmt.Errorf("didn't get the same policy back from the server \n%+v\n%+v", policy, policyServer)
	}

	updatedPolicy := policyServer
	updatedPolicy.Labels = map[string]string{"foo": "bar"}
	policyServer, err = policyClient.Update(ctx, updatedPolicy, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("error creating the policy '%v' (%v)", policy, err)
	}
	if defaultTierPolicyName != policyServer.Name {
		return fmt.Errorf("didn't get the same policy back from the server \n%+v\n%+v", policy, policyServer)
	}

	// For testing out Tiered Policy
	tierClient := client.ProjectcalicoV3().Tiers()
	order := float64(100.0)
	tier := &v3.Tier{
		ObjectMeta: metav1.ObjectMeta{Name: "net-sec"},
		Spec: v3.TierSpec{
			Order: &order,
		},
	}

	if _, err := tierClient.Create(ctx, tier, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("error creating tier '%v' (%v)", tier, err)
	}
	defer func() {
		_ = tierClient.Delete(ctx, "net-sec", metav1.DeleteOptions{})
	}()

	netSecPolicyName := "net-sec" + "." + name
	netSecPolicy := &v3.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: netSecPolicyName}, Spec: v3.NetworkPolicySpec{Tier: "net-sec"}}
	policyServer, err = policyClient.Create(ctx, netSecPolicy, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating the policy '%v' (%v)", netSecPolicy, err)
	}
	if netSecPolicyName != policyServer.Name {
		return fmt.Errorf("didn't get the same policy back from the server \n%+v\n%+v", policy, policyServer)
	}

	// Should be listing the policy under default tier.
	policies, err = policyClient.List(ctx, metav1.ListOptions{FieldSelector: "spec.tier=default"})
	if err != nil {
		return fmt.Errorf("error listing policies (%s)", err)
	}
	if len(policies.Items) != 1 {
		return fmt.Errorf("should have exactly one policy, had %v policies", len(policies.Items))
	}

	// Should be listing the policy under "net-sec" tier
	policies, err = policyClient.List(ctx, metav1.ListOptions{FieldSelector: "spec.tier=net-sec"})
	if err != nil {
		return fmt.Errorf("error listing policies (%s)", err)
	}
	if len(policies.Items) != 1 {
		return fmt.Errorf("should have exactly one policy, had %v policies", len(policies.Items))
	}

	// Should be listing all policy
	policies, err = policyClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing policies (%s)", err)
	}
	if len(policies.Items) != 2 {
		return fmt.Errorf("should have exactly two policies, had %v policies", len(policies.Items))
	}

	// Should be listing the policy under "net-sec" tier
	policies, err = policyClient.List(ctx, metav1.ListOptions{LabelSelector: "projectcalico.org/tier in (net-sec)"})
	if err != nil {
		return fmt.Errorf("error listing NetworkPolicies (%s)", err)
	}
	if len(policies.Items) != 1 {
		return fmt.Errorf("should have exactly one policy, had %v policies", len(policies.Items))
	}
	if policies.Items[0].Spec.Tier != "net-sec" {
		return fmt.Errorf("should have list policy from net-sec tier, had %s tier", policies.Items[0].Spec.Tier)
	}

	// Should be listing the policy under "net-sec" and "default tier
	policies, err = policyClient.List(ctx, metav1.ListOptions{LabelSelector: "projectcalico.org/tier in (default, net-sec)"})
	if err != nil {
		return fmt.Errorf("error listing NetworkPolicies (%s)", err)
	}
	if len(policies.Items) != 2 {
		return fmt.Errorf("should have exactly two policies, had %v policies", len(policies.Items))
	}

	policyServer, err = policyClient.Get(ctx, defaultTierPolicyName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting policy %s (%s)", name, err)
	}
	if name != policyServer.Name &&
		policy.ResourceVersion == policyServer.ResourceVersion {
		return fmt.Errorf("didn't get the same policy back from the server \n%+v\n%+v", policy, policyServer)
	}

	// Watch Test:
	opts := metav1.ListOptions{Watch: true}
	wIface, err := policyClient.Watch(ctx, opts)
	if err != nil {
		return fmt.Errorf("Error on watch")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for e := range wIface.ResultChan() {
			fmt.Println("Watch object: ", e)
			break
		}
	}()

	err = policyClient.Delete(ctx, defaultTierPolicyName, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("policy should be deleted (%s)", err)
	}

	err = policyClient.Delete(ctx, netSecPolicyName, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("policy should be deleted (%s)", err)
	}

	wg.Wait()
	return nil
}

// TestStagedgNetworkPolicyClient exercises the StagedNetworkPolicy client.
func TestStagedNetworkPolicyClient(t *testing.T) {
	const name = "test-networkpolicy"
	rootTestFunc := func() func(t *testing.T) {
		return func(t *testing.T) {
			client, shutdownServer := getFreshApiserverAndClient(t, func() runtime.Object {
				return &v3.NetworkPolicy{}
			})
			defer shutdownServer()
			if err := testStagedNetworkPolicyClient(client, name); err != nil {
				t.Fatal(err)
			}
		}
	}

	if !t.Run(name, rootTestFunc()) {
		t.Errorf("test-stagednetworkpolicy test failed")
	}
}

func testStagedNetworkPolicyClient(client calicoclient.Interface, name string) error {
	ns := "default"
	defaultTierPolicyName := "default" + "." + name
	policyClient := client.ProjectcalicoV3().StagedNetworkPolicies(ns)
	policy := &v3.StagedNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: defaultTierPolicyName},
		Spec:       v3.StagedNetworkPolicySpec{StagedAction: "Set", Selector: "foo == \"bar\""},
	}
	ctx := context.Background()

	// start from scratch
	policies, err := policyClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing policies (%s)", err)
	}
	if policies.Items == nil {
		return fmt.Errorf("Items field should not be set to nil")
	}
	if len(policies.Items) > 0 {
		return fmt.Errorf("policies should not exist on start, had %v policies", len(policies.Items))
	}

	// Test that we can create / update / delete policies using the non-tier prefixed name.
	stagedNetworkPolicy2 := &v3.StagedNetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name}}
	stagedNetworkPolicyServer, err := policyClient.Create(ctx, stagedNetworkPolicy2, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating the staged network policy '%v' (%v)", stagedNetworkPolicy2, err)
	}
	if name != stagedNetworkPolicyServer.Name {
		return fmt.Errorf("policy name prefix was defaulted by the apiserver on create: %v", stagedNetworkPolicyServer)
	}
	stagedNetworkPolicyServer.Name = name
	stagedNetworkPolicyServer.Labels = map[string]string{"foo": "bar"}
	stagedNetworkPolicyServer, err = policyClient.Update(ctx, stagedNetworkPolicyServer, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("error updating the policy '%v' (%v)", stagedNetworkPolicyServer, err)
	}
	if name != stagedNetworkPolicyServer.Name {
		return fmt.Errorf("policy name prefix was defaulted by the apiserver on update: %v", stagedNetworkPolicyServer)
	}
	err = policyClient.Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("error deleting the policy '%v' (%v)", name, err)
	}

	policyServer, err := policyClient.Create(ctx, policy, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating the policy '%v' (%v)", policy, err)
	}
	if defaultTierPolicyName != policyServer.Name {
		return fmt.Errorf("didn't get the same policy back from the server \n%+v\n%+v", policy, policyServer)
	}

	updatedPolicy := policyServer
	updatedPolicy.Labels = map[string]string{"foo": "bar"}
	policyServer, err = policyClient.Update(ctx, updatedPolicy, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("error creating the policy '%v' (%v)", policy, err)
	}
	if defaultTierPolicyName != policyServer.Name {
		return fmt.Errorf("didn't get the same policy back from the server \n%+v\n%+v", policy, policyServer)
	}

	// For testing out Tiered Policy
	tierClient := client.ProjectcalicoV3().Tiers()
	order := float64(100.0)
	tier := &v3.Tier{
		ObjectMeta: metav1.ObjectMeta{Name: "net-sec"},
		Spec: v3.TierSpec{
			Order: &order,
		},
	}

	if _, err := tierClient.Create(ctx, tier, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("error creating tier '%v' (%v)", tier, err)
	}
	defer func() {
		_ = tierClient.Delete(ctx, "net-sec", metav1.DeleteOptions{})
	}()

	netSecPolicyName := "net-sec" + "." + name
	netSecPolicy := &v3.StagedNetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: netSecPolicyName}, Spec: v3.StagedNetworkPolicySpec{StagedAction: "Set", Selector: "foo == \"bar\"", Tier: "net-sec"}}
	policyServer, err = policyClient.Create(ctx, netSecPolicy, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating the policy '%v' (%v)", netSecPolicy, err)
	}
	if netSecPolicyName != policyServer.Name {
		return fmt.Errorf("didn't get the same policy back from the server \n%+v\n%+v", policy, policyServer)
	}

	// Should be listing the policy under default tier.
	policies, err = policyClient.List(ctx, metav1.ListOptions{FieldSelector: "spec.tier=default"})
	if err != nil {
		return fmt.Errorf("error listing policies (%s)", err)
	}
	if len(policies.Items) != 1 {
		return fmt.Errorf("should have exactly one policy, had %v policies", len(policies.Items))
	}

	// Should be listing the policy under "net-sec" tier
	policies, err = policyClient.List(ctx, metav1.ListOptions{FieldSelector: "spec.tier=net-sec"})
	if err != nil {
		return fmt.Errorf("error listing policies (%s)", err)
	}
	if len(policies.Items) != 1 {
		return fmt.Errorf("should have exactly one policy, had %v policies", len(policies.Items))
	}

	// Should be listing all policies
	policies, err = policyClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing policies (%s)", err)
	}
	if len(policies.Items) != 2 {
		return fmt.Errorf("should have exactly two policies, had %v policies", len(policies.Items))
	}

	// Should be listing the policy under "net-sec" tier
	policies, err = policyClient.List(ctx, metav1.ListOptions{LabelSelector: "projectcalico.org/tier in (net-sec)"})
	if err != nil {
		return fmt.Errorf("error listing stagedGlobalNetworkPolicies (%s)", err)
	}
	if len(policies.Items) != 1 {
		return fmt.Errorf("should have exactly one policy, had %v policies", len(policies.Items))
	}
	if policies.Items[0].Spec.Tier != "net-sec" {
		return fmt.Errorf("should have list policy from net-sec tier, had %s tier", policies.Items[0].Spec.Tier)
	}

	// Should be listing the policy under "net-sec" and "default tier
	policies, err = policyClient.List(ctx, metav1.ListOptions{LabelSelector: "projectcalico.org/tier in (default, net-sec)"})
	if err != nil {
		return fmt.Errorf("error listing stagedGlobalNetworkPolicies (%s)", err)
	}
	if len(policies.Items) != 2 {
		return fmt.Errorf("should have exactly two policies, had %v policies", len(policies.Items))
	}

	policyServer, err = policyClient.Get(ctx, defaultTierPolicyName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting policy %s (%s)", name, err)
	}
	if defaultTierPolicyName != policyServer.Name &&
		policy.ResourceVersion == policyServer.ResourceVersion {
		return fmt.Errorf("didn't get the same policy back from the server \n%+v\n%+v", policy, policyServer)
	}

	// Watch Test:
	opts := metav1.ListOptions{Watch: true}
	wIface, err := policyClient.Watch(ctx, opts)
	if err != nil {
		return fmt.Errorf("Error on watch")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for e := range wIface.ResultChan() {
			fmt.Println("Watch object: ", e)
			break
		}
	}()

	err = policyClient.Delete(ctx, defaultTierPolicyName, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("policy should be deleted (%s)", err)
	}

	err = policyClient.Delete(ctx, netSecPolicyName, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("policy should be deleted (%s)", err)
	}

	wg.Wait()
	return nil
}

// TestTierClient exercises the Tier client.
func TestTierClient(t *testing.T) {
	const name = "test-tier"
	rootTestFunc := func() func(t *testing.T) {
		return func(t *testing.T) {
			client, shutdownServer := getFreshApiserverAndClient(t, func() runtime.Object {
				return &v3.Tier{}
			})
			defer shutdownServer()
			if err := testTierClient(client, name); err != nil {
				t.Fatal(err)
			}
		}
	}

	if !t.Run(name, rootTestFunc()) {
		t.Errorf("test-tier test failed")
	}
}

func testTierClient(client calicoclient.Interface, name string) error {
	tierClient := client.ProjectcalicoV3().Tiers()
	order := float64(100.0)
	tier := &v3.Tier{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v3.TierSpec{
			Order: &order,
		},
	}
	ctx := context.Background()

	// start from scratch
	tiers, err := tierClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing tiers (%s)", err)
	}
	if tiers.Items == nil {
		return fmt.Errorf("Items field should not be set to nil")
	}

	tierServer, err := tierClient.Create(ctx, tier, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating the tier '%v' (%v)", tier, err)
	}
	if name != tierServer.Name {
		return fmt.Errorf("didn't get the same tier back from the server \n%+v\n%+v", tier, tierServer)
	}

	_, err = tierClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing tiers (%s)", err)
	}

	tierServer, err = tierClient.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting tier %s (%s)", name, err)
	}
	if name != tierServer.Name &&
		tier.ResourceVersion == tierServer.ResourceVersion {
		return fmt.Errorf("didn't get the same tier back from the server \n%+v\n%+v", tier, tierServer)
	}

	err = tierClient.Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("tier should be deleted (%s)", err)
	}

	return nil
}

// TestGlobalNetworkPolicyClient exercises the GlobalNetworkPolicy client.
func TestGlobalNetworkPolicyClient(t *testing.T) {
	const name = "test-globalnetworkpolicy"
	rootTestFunc := func() func(t *testing.T) {
		return func(t *testing.T) {
			client, shutdownServer := getFreshApiserverAndClient(t, func() runtime.Object {
				return &v3.GlobalNetworkPolicy{}
			})
			defer shutdownServer()
			if err := testGlobalNetworkPolicyClient(client, name); err != nil {
				t.Fatal(err)
			}
		}
	}

	if !t.Run(name, rootTestFunc()) {
		t.Errorf("test-globalnetworkpolicy test failed")
	}
}

func testGlobalNetworkPolicyClient(client calicoclient.Interface, name string) error {
	globalNetworkPolicyClient := client.ProjectcalicoV3().GlobalNetworkPolicies()
	defaultTierPolicyName := "default" + "." + name
	globalNetworkPolicy := &v3.GlobalNetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: defaultTierPolicyName}}
	ctx := context.Background()

	// start from scratch
	globalNetworkPolicies, err := globalNetworkPolicyClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing globalNetworkPolicies (%s)", err)
	}
	if globalNetworkPolicies.Items == nil {
		return fmt.Errorf("Items field should not be set to nil")
	}

	// Test that we can create / update / delete policies using the non-tier prefixed name.
	globalNetworkPolicy2 := &v3.GlobalNetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name}}
	globalNetworkPolicyServer, err := globalNetworkPolicyClient.Create(ctx, globalNetworkPolicy2, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating the globalNetworkPolicy '%v' (%v)", globalNetworkPolicy2, err)
	}
	if defaultTierPolicyName == globalNetworkPolicyServer.Name {
		return fmt.Errorf("policy name prefix was defaulted by the apiserver on create: %v", globalNetworkPolicyServer)
	}
	globalNetworkPolicyServer.Name = name
	globalNetworkPolicyServer.Labels = map[string]string{"foo": "bar"}
	globalNetworkPolicyServer, err = globalNetworkPolicyClient.Update(ctx, globalNetworkPolicyServer, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("error updating the policy '%v' (%v)", globalNetworkPolicyServer, err)
	}
	if defaultTierPolicyName == globalNetworkPolicyServer.Name {
		return fmt.Errorf("policy name prefix was defaulted by the apiserver on update: %v", globalNetworkPolicyServer)
	}
	err = globalNetworkPolicyClient.Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("error deleting the policy '%v' (%v)", name, err)
	}

	// Now use the tier prefixed name.
	globalNetworkPolicyServer, err = globalNetworkPolicyClient.Create(ctx, globalNetworkPolicy, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating the globalNetworkPolicy '%v' (%v)", globalNetworkPolicy, err)
	}
	if defaultTierPolicyName != globalNetworkPolicyServer.Name {
		return fmt.Errorf("didn't get the same globalNetworkPolicy back from the server \n%+v\n%+v", globalNetworkPolicy, globalNetworkPolicyServer)
	}

	// For testing out Tiered Policy
	tierClient := client.ProjectcalicoV3().Tiers()
	order := float64(100.0)
	tier := &v3.Tier{
		ObjectMeta: metav1.ObjectMeta{Name: "net-sec"},
		Spec: v3.TierSpec{
			Order: &order,
		},
	}

	if _, err := tierClient.Create(ctx, tier, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("error creating tier '%v' (%v)", tier, err)
	}
	defer func() {
		_ = tierClient.Delete(ctx, "net-sec", metav1.DeleteOptions{})
	}()

	netSecPolicyName := "net-sec" + "." + name
	netSecPolicy := &v3.GlobalNetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: netSecPolicyName}, Spec: v3.GlobalNetworkPolicySpec{Tier: "net-sec"}}
	globalNetworkPolicyServer, err = globalNetworkPolicyClient.Create(ctx, netSecPolicy, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating the policy '%v' (%v)", netSecPolicy, err)
	}
	if netSecPolicyName != globalNetworkPolicyServer.Name {
		return fmt.Errorf("didn't get the same policy back from the server \n%+v\n%+v", netSecPolicy, globalNetworkPolicyServer)
	}

	// Should be listing the policy under "default" tier
	globalNetworkPolicies, err = globalNetworkPolicyClient.List(ctx, metav1.ListOptions{FieldSelector: "spec.tier=default"})
	if err != nil {
		return fmt.Errorf("error listing globalNetworkPolicies (%s)", err)
	}
	if len(globalNetworkPolicies.Items) != 1 {
		return fmt.Errorf("should have exactly one policy, had %v policies", len(globalNetworkPolicies.Items))
	}

	// Should be listing the policy under "net-sec" tier
	globalNetworkPolicies, err = globalNetworkPolicyClient.List(ctx, metav1.ListOptions{FieldSelector: "spec.tier=net-sec"})
	if err != nil {
		return fmt.Errorf("error listing globalNetworkPolicies (%s)", err)
	}
	if len(globalNetworkPolicies.Items) != 1 {
		return fmt.Errorf("should have exactly one policy, had %v policies", len(globalNetworkPolicies.Items))
	}

	// Should be listing all policies
	globalNetworkPolicies, err = globalNetworkPolicyClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing globalNetworkPolicies (%s)", err)
	}
	if len(globalNetworkPolicies.Items) != 2 {
		return fmt.Errorf("should have exactly two policies, had %v policies", len(globalNetworkPolicies.Items))
	}

	// Should be listing the policy under "net-sec" tier
	globalNetworkPolicies, err = globalNetworkPolicyClient.List(ctx, metav1.ListOptions{LabelSelector: "projectcalico.org/tier in (net-sec)"})
	if err != nil {
		return fmt.Errorf("error listing stagedGlobalNetworkPolicies (%s)", err)
	}
	if len(globalNetworkPolicies.Items) != 1 {
		return fmt.Errorf("should have exactly one policy, had %v policies", len(globalNetworkPolicies.Items))
	}
	if globalNetworkPolicies.Items[0].Spec.Tier != "net-sec" {
		return fmt.Errorf("should have list policy from net-sec tier, had %s tier", globalNetworkPolicies.Items[0].Spec.Tier)
	}

	// Should be listing the policy under "net-sec" and "default tier
	globalNetworkPolicies, err = globalNetworkPolicyClient.List(ctx, metav1.ListOptions{LabelSelector: "projectcalico.org/tier in (default, net-sec)"})
	if err != nil {
		return fmt.Errorf("error listing stagedGlobalNetworkPolicies (%s)", err)
	}
	if len(globalNetworkPolicies.Items) != 2 {
		return fmt.Errorf("should have exactly two policies, had %v policies", len(globalNetworkPolicies.Items))
	}

	globalNetworkPolicyServer, err = globalNetworkPolicyClient.Get(ctx, defaultTierPolicyName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting globalNetworkPolicy %s (%s)", name, err)
	}
	if name != globalNetworkPolicyServer.Name &&
		globalNetworkPolicy.ResourceVersion == globalNetworkPolicyServer.ResourceVersion {
		return fmt.Errorf("didn't get the same globalNetworkPolicy back from the server \n%+v\n%+v", globalNetworkPolicy, globalNetworkPolicyServer)
	}

	err = globalNetworkPolicyClient.Delete(ctx, defaultTierPolicyName, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("globalNetworkPolicy should be deleted (%s)", err)
	}

	err = globalNetworkPolicyClient.Delete(ctx, netSecPolicyName, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("policy should be deleted (%s)", err)
	}

	return nil
}

// TestStagedGlobalNetworkPolicyClient exercises the StagedGlobalNetworkPolicy client.
func TestStagedGlobalNetworkPolicyClient(t *testing.T) {
	const name = "test-stagedglobalnetworkpolicy"
	rootTestFunc := func() func(t *testing.T) {
		return func(t *testing.T) {
			client, shutdownServer := getFreshApiserverAndClient(t, func() runtime.Object {
				return &v3.StagedGlobalNetworkPolicy{}
			})
			defer shutdownServer()
			if err := testStagedGlobalNetworkPolicyClient(client, name); err != nil {
				t.Fatal(err)
			}
		}
	}

	if !t.Run(name, rootTestFunc()) {
		t.Errorf("test-Stagedglobalnetworkpolicy test failed")
	}
}

func testStagedGlobalNetworkPolicyClient(client calicoclient.Interface, name string) error {
	stagedGlobalNetworkPolicyClient := client.ProjectcalicoV3().StagedGlobalNetworkPolicies()
	defaultTierPolicyName := name
	stagedGlobalNetworkPolicy := &v3.StagedGlobalNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v3.StagedGlobalNetworkPolicySpec{StagedAction: "Set", Selector: "foo == \"bar\""},
	}
	ctx := context.Background()

	// start from scratch
	stagedGlobalNetworkPolicies, err := stagedGlobalNetworkPolicyClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing stagedglobalNetworkPolicies (%s)", err)
	}
	if stagedGlobalNetworkPolicies.Items == nil {
		return fmt.Errorf("Items field should not be set to nil")
	}

	// Test that we can create / update / delete policies using the non-tier prefixed name.
	stagedGlobalNetworkPolicy2 := &v3.StagedGlobalNetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name}}
	stagedGlobalNetworkPolicyServer, err := stagedGlobalNetworkPolicyClient.Create(ctx, stagedGlobalNetworkPolicy2, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating the staged globalNetworkPolicy '%v' (%v)", stagedGlobalNetworkPolicy2, err)
	}
	if defaultTierPolicyName != stagedGlobalNetworkPolicyServer.Name {
		return fmt.Errorf("policy name prefix wasn't defaulted by the apiserver on create: %v", stagedGlobalNetworkPolicyServer)
	}
	stagedGlobalNetworkPolicyServer.Name = name
	stagedGlobalNetworkPolicyServer.Labels = map[string]string{"foo": "bar"}
	stagedGlobalNetworkPolicyServer, err = stagedGlobalNetworkPolicyClient.Update(ctx, stagedGlobalNetworkPolicyServer, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("error updating the policy '%v' (%v)", stagedGlobalNetworkPolicyServer, err)
	}
	if defaultTierPolicyName != stagedGlobalNetworkPolicyServer.Name {
		return fmt.Errorf("policy name prefix wasn't defaulted by the apiserver on update: %v", stagedGlobalNetworkPolicyServer)
	}
	err = stagedGlobalNetworkPolicyClient.Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("error deleting the policy '%v' (%v)", name, err)
	}

	stagedGlobalNetworkPolicyServer, err = stagedGlobalNetworkPolicyClient.Create(ctx, stagedGlobalNetworkPolicy, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating the stagedGlobalNetworkPolicy '%v' (%v)", stagedGlobalNetworkPolicy, err)
	}
	if defaultTierPolicyName != stagedGlobalNetworkPolicyServer.Name {
		return fmt.Errorf("didn't get the same stagedGlobalNetworkPolicy back from the server \n%+v\n%+v", stagedGlobalNetworkPolicy, stagedGlobalNetworkPolicyServer)
	}

	// For testing out Tiered Policy
	tierClient := client.ProjectcalicoV3().Tiers()
	order := float64(100.0)
	tier := &v3.Tier{
		ObjectMeta: metav1.ObjectMeta{Name: "net-sec"},
		Spec: v3.TierSpec{
			Order: &order,
		},
	}

	if _, err := tierClient.Create(ctx, tier, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("error creating tier '%v' (%v)", tier, err)
	}
	defer func() {
		_ = tierClient.Delete(ctx, "net-sec", metav1.DeleteOptions{})
	}()

	netSecPolicyName := "net-sec" + "." + name
	netSecPolicy := &v3.StagedGlobalNetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: netSecPolicyName}, Spec: v3.StagedGlobalNetworkPolicySpec{StagedAction: "Set", Selector: "foo == \"bar\"", Tier: "net-sec"}}
	stagedGlobalNetworkPolicyServer, err = stagedGlobalNetworkPolicyClient.Create(ctx, netSecPolicy, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating the policy '%v' (%v)", netSecPolicy, err)
	}
	if netSecPolicyName != stagedGlobalNetworkPolicyServer.Name {
		return fmt.Errorf("didn't get the same policy back from the server \n%+v\n%+v", netSecPolicy, stagedGlobalNetworkPolicyServer)
	}

	// Should be listing the policy under "default" tier
	stagedGlobalNetworkPolicies, err = stagedGlobalNetworkPolicyClient.List(ctx, metav1.ListOptions{FieldSelector: "spec.tier=default"})
	if err != nil {
		return fmt.Errorf("error listing stagedGlobalNetworkPolicies (%s)", err)
	}
	if len(stagedGlobalNetworkPolicies.Items) != 1 {
		return fmt.Errorf("should have exactly one policy, had %v policies", len(stagedGlobalNetworkPolicies.Items))
	}

	// Should be listing the policy under "net-sec" tier
	stagedGlobalNetworkPolicies, err = stagedGlobalNetworkPolicyClient.List(ctx, metav1.ListOptions{FieldSelector: "spec.tier=net-sec"})
	if err != nil {
		return fmt.Errorf("error listing stagedGlobalNetworkPolicies (%s)", err)
	}
	if len(stagedGlobalNetworkPolicies.Items) != 1 {
		return fmt.Errorf("should have exactly one policy, had %v policies", len(stagedGlobalNetworkPolicies.Items))
	}

	// Should be listing all policies
	stagedGlobalNetworkPolicies, err = stagedGlobalNetworkPolicyClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing stagedGlobalNetworkPolicies (%s)", err)
	}
	if len(stagedGlobalNetworkPolicies.Items) != 2 {
		return fmt.Errorf("should have exactly two policies, had %v policies", len(stagedGlobalNetworkPolicies.Items))
	}

	// Should be listing the policy under "net-sec" tier
	stagedGlobalNetworkPolicies, err = stagedGlobalNetworkPolicyClient.List(ctx, metav1.ListOptions{LabelSelector: "projectcalico.org/tier in (net-sec)"})
	if err != nil {
		return fmt.Errorf("error listing stagedGlobalNetworkPolicies (%s)", err)
	}
	if len(stagedGlobalNetworkPolicies.Items) != 1 {
		return fmt.Errorf("should have exactly one policy, had %v policies", len(stagedGlobalNetworkPolicies.Items))
	}
	if stagedGlobalNetworkPolicies.Items[0].Spec.Tier != "net-sec" {
		return fmt.Errorf("should have list policy from net-sec tier, had %s tier", stagedGlobalNetworkPolicies.Items[0].Spec.Tier)
	}

	// Should be listing the policy under "net-sec" and "default tier
	stagedGlobalNetworkPolicies, err = stagedGlobalNetworkPolicyClient.List(ctx, metav1.ListOptions{LabelSelector: "projectcalico.org/tier in (default, net-sec)"})
	if err != nil {
		return fmt.Errorf("error listing stagedGlobalNetworkPolicies (%s)", err)
	}
	if len(stagedGlobalNetworkPolicies.Items) != 2 {
		return fmt.Errorf("should have exactly two policies, had %v policies", len(stagedGlobalNetworkPolicies.Items))
	}

	stagedGlobalNetworkPolicyServer, err = stagedGlobalNetworkPolicyClient.Get(ctx, defaultTierPolicyName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting stagedGlobalNetworkPolicy %s (%s)", name, err)
	}
	if name != stagedGlobalNetworkPolicyServer.Name &&
		stagedGlobalNetworkPolicy.ResourceVersion == stagedGlobalNetworkPolicyServer.ResourceVersion {
		return fmt.Errorf("didn't get the same stagedGlobalNetworkPolicy back from the server \n%+v\n%+v", stagedGlobalNetworkPolicy, stagedGlobalNetworkPolicyServer)
	}

	err = stagedGlobalNetworkPolicyClient.Delete(ctx, defaultTierPolicyName, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("stagedGlobalNetworkPolicy should be deleted (%s)", err)
	}

	err = stagedGlobalNetworkPolicyClient.Delete(ctx, netSecPolicyName, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("policy should be deleted (%s)", err)
	}

	return nil
}

// TestGlobalNetworkSetClient exercises the GlobalNetworkSet client.
func TestGlobalNetworkSetClient(t *testing.T) {
	const name = "test-globalnetworkset"
	rootTestFunc := func() func(t *testing.T) {
		return func(t *testing.T) {
			client, shutdownServer := getFreshApiserverAndClient(t, func() runtime.Object {
				return &v3.GlobalNetworkSet{}
			})
			defer shutdownServer()
			if err := testGlobalNetworkSetClient(client, name); err != nil {
				t.Fatal(err)
			}
		}
	}

	if !t.Run(name, rootTestFunc()) {
		t.Errorf("test-globalnetworkset test failed")
	}
}

func testGlobalNetworkSetClient(client calicoclient.Interface, name string) error {
	globalNetworkSetClient := client.ProjectcalicoV3().GlobalNetworkSets()
	globalNetworkSet := &v3.GlobalNetworkSet{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
	ctx := context.Background()

	// start from scratch
	globalNetworkSets, err := globalNetworkSetClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing globalNetworkSets (%s)", err)
	}
	if globalNetworkSets.Items == nil {
		return fmt.Errorf("Items field should not be set to nil")
	}

	globalNetworkSetServer, err := globalNetworkSetClient.Create(ctx, globalNetworkSet, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating the globalNetworkSet '%v' (%v)", globalNetworkSet, err)
	}
	if name != globalNetworkSetServer.Name {
		return fmt.Errorf("didn't get the same globalNetworkSet back from the server \n%+v\n%+v", globalNetworkSet, globalNetworkSetServer)
	}

	_, err = globalNetworkSetClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing globalNetworkSets (%s)", err)
	}

	globalNetworkSetServer, err = globalNetworkSetClient.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting globalNetworkSet %s (%s)", name, err)
	}
	if name != globalNetworkSetServer.Name &&
		globalNetworkSet.ResourceVersion == globalNetworkSetServer.ResourceVersion {
		return fmt.Errorf("didn't get the same globalNetworkSet back from the server \n%+v\n%+v", globalNetworkSet, globalNetworkSetServer)
	}

	err = globalNetworkSetClient.Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("globalNetworkSet should be deleted (%s)", err)
	}

	return nil
}

// TestNetworkSetClient exercises the NetworkSet client.
func TestNetworkSetClient(t *testing.T) {
	const name = "test-networkset"
	rootTestFunc := func() func(t *testing.T) {
		return func(t *testing.T) {
			client, shutdownServer := getFreshApiserverAndClient(t, func() runtime.Object {
				return &v3.NetworkSet{}
			})
			defer shutdownServer()
			if err := testNetworkSetClient(client, name); err != nil {
				t.Fatal(err)
			}
		}
	}

	if !t.Run(name, rootTestFunc()) {
		t.Errorf("test-networkset test failed")
	}
}

func testNetworkSetClient(client calicoclient.Interface, name string) error {
	ns := "default"
	networkSetClient := client.ProjectcalicoV3().NetworkSets(ns)
	networkSet := &v3.NetworkSet{ObjectMeta: metav1.ObjectMeta{Name: name}}
	ctx := context.Background()

	// start from scratch
	networkSets, err := networkSetClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing networkSets (%s)", err)
	}
	if networkSets.Items == nil {
		return fmt.Errorf("Items field should not be set to nil")
	}
	if len(networkSets.Items) > 0 {
		return fmt.Errorf("networkSets should not exist on start, had %v networkSets", len(networkSets.Items))
	}

	networkSetServer, err := networkSetClient.Create(ctx, networkSet, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating the networkSet '%v' (%v)", networkSet, err)
	}

	updatedNetworkSet := networkSetServer
	updatedNetworkSet.Labels = map[string]string{"foo": "bar"}
	_, err = networkSetClient.Update(ctx, updatedNetworkSet, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("error updating the networkSet '%v' (%v)", networkSet, err)
	}

	// Should be listing the networkSet.
	networkSets, err = networkSetClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing networkSets (%s)", err)
	}
	if len(networkSets.Items) != 1 {
		return fmt.Errorf("should have exactly one networkSet, had %v networkSets", len(networkSets.Items))
	}

	networkSetServer, err = networkSetClient.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting networkSet %s (%s)", name, err)
	}
	if name != networkSetServer.Name &&
		networkSet.ResourceVersion == networkSetServer.ResourceVersion {
		return fmt.Errorf("didn't get the same networkSet back from the server \n%+v\n%+v", networkSet, networkSetServer)
	}

	// Watch Test:
	opts := metav1.ListOptions{Watch: true}
	wIface, err := networkSetClient.Watch(ctx, opts)
	if err != nil {
		return fmt.Errorf("Error on watch")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for e := range wIface.ResultChan() {
			fmt.Println("Watch object: ", e)
			break
		}
	}()

	err = networkSetClient.Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("networkSet should be deleted (%s)", err)
	}

	wg.Wait()
	return nil
}

// TestIPReservationClient exercises the IPReservation client.
func TestIPReservationClient(t *testing.T) {
	const name = "test-ipreservation"
	rootTestFunc := func() func(t *testing.T) {
		return func(t *testing.T) {
			client, shutdownServer := getFreshApiserverAndClient(t, func() runtime.Object {
				return &v3.IPReservation{}
			})
			defer shutdownServer()
			if err := testIPReservationClient(client, name); err != nil {
				t.Fatal(err)
			}
		}
	}

	if !t.Run(name, rootTestFunc()) {
		t.Errorf("test-ipreservation test failed")
	}
}

func testIPReservationClient(client calicoclient.Interface, name string) error {
	ipreservationClient := client.ProjectcalicoV3().IPReservations()
	ipreservation := &v3.IPReservation{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v3.IPReservationSpec{
			ReservedCIDRs: []string{"192.168.0.0/16"},
		},
	}
	ctx := context.Background()

	// start from scratch
	ipreservations, err := ipreservationClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing ipreservations (%s)", err)
	}
	if ipreservations.Items == nil {
		return fmt.Errorf("items field should not be set to nil")
	}

	ipreservationServer, err := ipreservationClient.Create(ctx, ipreservation, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating the ipreservation '%v' (%v)", ipreservation, err)
	}
	if name != ipreservationServer.Name {
		return fmt.Errorf("didn't get the same ipreservation back from the server \n%+v\n%+v", ipreservation, ipreservationServer)
	}

	_, err = ipreservationClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing ipreservations (%s)", err)
	}

	ipreservationServer, err = ipreservationClient.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting ipreservation %s (%s)", name, err)
	}
	if name != ipreservationServer.Name &&
		ipreservation.ResourceVersion == ipreservationServer.ResourceVersion {
		return fmt.Errorf("didn't get the same ipreservation back from the server \n%+v\n%+v", ipreservation, ipreservationServer)
	}

	err = ipreservationClient.Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("ipreservation should be deleted (%s)", err)
	}

	return nil
}

// TestHostEndpointClient exercises the HostEndpoint client.
func TestHostEndpointClient(t *testing.T) {
	const name = "test-hostendpoint"
	client, shutdownServer := getFreshApiserverAndClient(t, func() runtime.Object {
		return &v3.HostEndpoint{}
	})
	defer shutdownServer()
	defer func() {
		_ = deleteHostEndpointClient(client, name)
	}()
	rootTestFunc := func() func(t *testing.T) {
		return func(t *testing.T) {
			client, shutdownServer := getFreshApiserverAndClient(t, func() runtime.Object {
				return &v3.HostEndpoint{}
			})
			defer shutdownServer()
			if err := testHostEndpointClient(client, name); err != nil {
				t.Fatal(err)
			}
		}
	}

	if !t.Run(name, rootTestFunc()) {
		t.Errorf("test-hostendpoint test failed")
	}
}

func createTestHostEndpoint(name string, ip string, node string) *v3.HostEndpoint {
	hostEndpoint := &v3.HostEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
	hostEndpoint.Spec.ExpectedIPs = []string{ip}
	hostEndpoint.Spec.Node = node

	return hostEndpoint
}

func deleteHostEndpointClient(client calicoclient.Interface, name string) error {
	hostEndpointClient := client.ProjectcalicoV3().HostEndpoints()
	ctx := context.Background()

	return hostEndpointClient.Delete(ctx, name, metav1.DeleteOptions{})
}

func testHostEndpointClient(client calicoclient.Interface, name string) error {
	hostEndpointClient := client.ProjectcalicoV3().HostEndpoints()

	hostEndpoint := createTestHostEndpoint(name, "192.168.0.1", "test-node")
	ctx := context.Background()

	// start from scratch
	hostEndpoints, err := hostEndpointClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing hostEndpoints (%s)", err)
	}
	if hostEndpoints.Items == nil {
		return fmt.Errorf("Items field should not be set to nil")
	}

	hostEndpointServer, err := hostEndpointClient.Create(ctx, hostEndpoint, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating the hostEndpoint '%v' (%v)", hostEndpoint, err)
	}
	if name != hostEndpointServer.Name {
		return fmt.Errorf("didn't get the same hostEndpoint back from the server \n%+v\n%+v", hostEndpoint, hostEndpointServer)
	}

	hostEndpoints, err = hostEndpointClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing hostEndpoints (%s)", err)
	}
	if len(hostEndpoints.Items) != 1 {
		return fmt.Errorf("expected 1 hostEndpoint entry, got %d", len(hostEndpoints.Items))
	}

	hostEndpointServer, err = hostEndpointClient.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting hostEndpoint %s (%s)", name, err)
	}
	if name != hostEndpointServer.Name &&
		hostEndpoint.ResourceVersion == hostEndpointServer.ResourceVersion {
		return fmt.Errorf("didn't get the same hostEndpoint back from the server \n%+v\n%+v", hostEndpoint, hostEndpointServer)
	}

	err = hostEndpointClient.Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("hostEndpoint should be deleted (%s)", err)
	}

	// Test watch
	w, err := client.ProjectcalicoV3().HostEndpoints().Watch(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error watching HostEndpoints (%s)", err)
	}
	var events []watch.Event
	done := sync.WaitGroup{}
	done.Add(1)
	timeout := time.After(500 * time.Millisecond)
	var timeoutErr error
	// watch for 2 events
	go func() {
		defer done.Done()
		for i := 0; i < 2; i++ {
			select {
			case e := <-w.ResultChan():
				events = append(events, e)
			case <-timeout:
				timeoutErr = fmt.Errorf("timed out waiting for events")
				return
			}
		}
	}()

	// Create two HostEndpoints
	for i := 0; i < 2; i++ {
		hep := createTestHostEndpoint(fmt.Sprintf("hep%d", i), "192.168.0.1", "test-node")
		_, err = hostEndpointClient.Create(ctx, hep, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("error creating hostEndpoint '%v' (%v)", hep, err)
		}
	}

	done.Wait()
	if timeoutErr != nil {
		return timeoutErr
	}
	if len(events) != 2 {
		return fmt.Errorf("expected 2 watch events got %d", len(events))
	}

	return nil
}

// TestIPPoolClient exercises the IPPool client.
func TestIPPoolClient(t *testing.T) {
	const name = "test-ippool"
	rootTestFunc := func() func(t *testing.T) {
		return func(t *testing.T) {
			client, shutdownServer := getFreshApiserverAndClient(t, func() runtime.Object {
				return &v3.IPPool{}
			})
			defer shutdownServer()
			if err := testIPPoolClient(client, name); err != nil {
				t.Fatal(err)
			}
		}
	}

	if !t.Run(name, rootTestFunc()) {
		t.Errorf("test-ippool test failed")
	}
}

func testIPPoolClient(client calicoclient.Interface, name string) error {
	ippoolClient := client.ProjectcalicoV3().IPPools()
	ippool := &v3.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v3.IPPoolSpec{
			CIDR: "192.168.0.0/16",
		},
	}
	ctx := context.Background()

	// start from scratch
	ippools, err := ippoolClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing ippools (%s)", err)
	}
	if ippools.Items == nil {
		return fmt.Errorf("Items field should not be set to nil")
	}

	ippoolServer, err := ippoolClient.Create(ctx, ippool, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating the ippool '%v' (%v)", ippool, err)
	}
	if name != ippoolServer.Name {
		return fmt.Errorf("didn't get the same ippool back from the server \n%+v\n%+v", ippool, ippoolServer)
	}

	_, err = ippoolClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing ippools (%s)", err)
	}

	ippoolServer, err = ippoolClient.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting ippool %s (%s)", name, err)
	}
	if name != ippoolServer.Name &&
		ippool.ResourceVersion == ippoolServer.ResourceVersion {
		return fmt.Errorf("didn't get the same ippool back from the server \n%+v\n%+v", ippool, ippoolServer)
	}

	err = ippoolClient.Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("ippool should be deleted (%s)", err)
	}

	return nil
}

// TestBGPConfigurationClient exercises the BGPConfiguration client.
func TestBGPConfigurationClient(t *testing.T) {
	const name = "test-bgpconfig"
	rootTestFunc := func() func(t *testing.T) {
		return func(t *testing.T) {
			client, shutdownServer := getFreshApiserverAndClient(t, func() runtime.Object {
				return &v3.BGPConfiguration{}
			})
			defer shutdownServer()
			if err := testBGPConfigurationClient(client, name); err != nil {
				t.Fatal(err)
			}
		}
	}

	if !t.Run(name, rootTestFunc()) {
		t.Errorf("test-bgpconfig test failed")
	}
}

func testBGPConfigurationClient(client calicoclient.Interface, name string) error {
	bgpConfigClient := client.ProjectcalicoV3().BGPConfigurations()
	resName := "bgpconfig-test"
	bgpConfig := &v3.BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: resName},
		Spec: v3.BGPConfigurationSpec{
			LogSeverityScreen: "Info",
		},
	}
	ctx := context.Background()

	// start from scratch
	bgpConfigList, err := bgpConfigClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing bgpConfiguration (%s)", err)
	}
	if bgpConfigList.Items == nil {
		return fmt.Errorf("Items field should not be set to nil")
	}

	bgpRes, err := bgpConfigClient.Create(ctx, bgpConfig, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating the bgpConfiguration '%v' (%v)", bgpConfig, err)
	}
	if resName != bgpRes.Name {
		return fmt.Errorf("didn't get the same bgpConfig back from server\n%+v\n%+v", bgpConfig, bgpRes)
	}

	_, err = bgpConfigClient.Get(ctx, resName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting bgpConfiguration %s (%s)", resName, err)
	}

	err = bgpConfigClient.Delete(ctx, resName, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("BGPConfiguration should be deleted (%s)", err)
	}

	return nil
}

// TestBGPPeerClient exercises the BGPPeer client.
func TestBGPPeerClient(t *testing.T) {
	const name = "test-bgppeer"
	rootTestFunc := func() func(t *testing.T) {
		return func(t *testing.T) {
			client, shutdownServer := getFreshApiserverAndClient(t, func() runtime.Object {
				return &v3.BGPPeer{}
			})
			defer shutdownServer()
			if err := testBGPPeerClient(client, name); err != nil {
				t.Fatal(err)
			}
		}
	}

	if !t.Run(name, rootTestFunc()) {
		t.Errorf("test-bgppeer test failed")
	}
}

func testBGPPeerClient(client calicoclient.Interface, name string) error {
	bgpPeerClient := client.ProjectcalicoV3().BGPPeers()
	resName := "bgppeer-test"
	bgpPeer := &v3.BGPPeer{
		ObjectMeta: metav1.ObjectMeta{Name: resName},
		Spec: v3.BGPPeerSpec{
			Node:     "node1",
			PeerIP:   "10.0.0.1",
			ASNumber: numorstring.ASNumber(6512),
		},
	}
	ctx := context.Background()

	// start from scratch
	bgpPeerList, err := bgpPeerClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing bgpPeer (%s)", err)
	}
	if bgpPeerList.Items == nil {
		return fmt.Errorf("Items field should not be set to nil")
	}

	bgpRes, err := bgpPeerClient.Create(ctx, bgpPeer, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating the bgpPeer '%v' (%v)", bgpPeer, err)
	}
	if resName != bgpRes.Name {
		return fmt.Errorf("didn't get the same bgpPeer back from server\n%+v\n%+v", bgpPeer, bgpRes)
	}

	_, err = bgpPeerClient.Get(ctx, resName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting bgpPeer %s (%s)", resName, err)
	}

	err = bgpPeerClient.Delete(ctx, resName, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("BGPPeer should be deleted (%s)", err)
	}

	return nil
}

// TestProfileClient exercises the Profile client.
func TestProfileClient(t *testing.T) {
	// This matches the namespace that is created at test setup time in the Makefile.
	// TODO(doublek): Note that this currently only works for KDD mode.
	const name = "kns.namespace-1"
	rootTestFunc := func() func(t *testing.T) {
		return func(t *testing.T) {
			client, shutdownServer := getFreshApiserverAndClient(t, func() runtime.Object {
				return &v3.Profile{}
			})
			defer shutdownServer()
			if err := testProfileClient(client, name); err != nil {
				t.Fatal(err)
			}
		}
	}

	if !t.Run(name, rootTestFunc()) {
		t.Errorf("test-profile test failed")
	}
}

func testProfileClient(client calicoclient.Interface, name string) error {
	profileClient := client.ProjectcalicoV3().Profiles()
	profile := &v3.Profile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v3.ProfileSpec{
			LabelsToApply: map[string]string{
				"aa": "bb",
			},
		},
	}
	ctx := context.Background()

	// start from scratch
	profileList, err := profileClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing profile (%s)", err)
	}
	if profileList.Items == nil {
		return fmt.Errorf("Items field should not be set to nil")
	}

	// Profile creation is not supported.
	_, err = profileClient.Create(ctx, profile, metav1.CreateOptions{})
	if err == nil {
		return fmt.Errorf("profile should not be allowed to be created'%v' (%v)", profile, err)
	}

	profileRes, err := profileClient.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting profile %s (%s)", name, err)
	}

	if name != profileRes.Name {
		return fmt.Errorf("didn't get the same profile back from server\n%+v\n%+v", profile, profileRes)
	}

	// Profile deletion is not supported.
	err = profileClient.Delete(ctx, name, metav1.DeleteOptions{})
	if err == nil {
		return fmt.Errorf("Profile cannot be deleted (%s)", err)
	}

	return nil
}

// TestFelixConfigurationClient exercises the FelixConfiguration client.
func TestFelixConfigurationClient(t *testing.T) {
	const name = "test-felixconfig"
	rootTestFunc := func() func(t *testing.T) {
		return func(t *testing.T) {
			client, shutdownServer := getFreshApiserverAndClient(t, func() runtime.Object {
				return &v3.FelixConfiguration{}
			})
			defer shutdownServer()
			if err := testFelixConfigurationClient(client, name); err != nil {
				t.Fatal(err)
			}
		}
	}

	if !t.Run(name, rootTestFunc()) {
		t.Errorf("test-felixConfig test failed")
	}
}

func testFelixConfigurationClient(client calicoclient.Interface, name string) error {
	felixConfigClient := client.ProjectcalicoV3().FelixConfigurations()
	ptrTrue := true
	ptrInt := 1432
	felixConfig := &v3.FelixConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v3.FelixConfigurationSpec{
			UseInternalDataplaneDriver: &ptrTrue,
			DataplaneDriver:            "test-dataplane-driver",
			MetadataPort:               &ptrInt,
		},
	}
	ctx := context.Background()

	// start from scratch
	felixConfigs, err := felixConfigClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing felixConfigs (%s)", err)
	}
	if felixConfigs.Items == nil {
		return fmt.Errorf("Items field should not be set to nil")
	}

	felixConfigServer, err := felixConfigClient.Create(ctx, felixConfig, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating the felixConfig '%v' (%v)", felixConfig, err)
	}
	if name != felixConfigServer.Name {
		return fmt.Errorf("didn't get the same felixConfig back from the server \n%+v\n%+v", felixConfig, felixConfigServer)
	}

	_, err = felixConfigClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing felixConfigs (%s)", err)
	}

	felixConfigServer, err = felixConfigClient.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting felixConfig %s (%s)", name, err)
	}
	if name != felixConfigServer.Name &&
		felixConfig.ResourceVersion == felixConfigServer.ResourceVersion {
		return fmt.Errorf("didn't get the same felixConfig back from the server \n%+v\n%+v", felixConfig, felixConfigServer)
	}

	err = felixConfigClient.Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("felixConfig should be deleted (%s)", err)
	}

	return nil
}

// TestKubeControllersConfigurationClient exercises the KubeControllersConfiguration client.
func TestKubeControllersConfigurationClient(t *testing.T) {
	const name = "test-kubecontrollersconfig"
	rootTestFunc := func() func(t *testing.T) {
		return func(t *testing.T) {
			client, shutdownServer := getFreshApiserverAndClient(t, func() runtime.Object {
				return &v3.KubeControllersConfiguration{}
			})
			defer shutdownServer()
			if err := testKubeControllersConfigurationClient(client); err != nil {
				t.Fatal(err)
			}
		}
	}

	if !t.Run(name, rootTestFunc()) {
		t.Errorf("test-kubecontrollersconfig test failed")
	}
}

func testKubeControllersConfigurationClient(client calicoclient.Interface) error {
	kubeControllersConfigClient := client.ProjectcalicoV3().KubeControllersConfigurations()
	kubeControllersConfig := &v3.KubeControllersConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Status: v3.KubeControllersConfigurationStatus{
			RunningConfig: v3.KubeControllersConfigurationSpec{
				Controllers: v3.ControllersConfig{
					Node: &v3.NodeControllerConfig{
						SyncLabels: v3.Enabled,
					},
				},
			},
		},
	}
	ctx := context.Background()

	// start from scratch
	kubeControllersConfigs, err := kubeControllersConfigClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing kubeControllersConfigs (%s)", err)
	}
	if kubeControllersConfigs.Items == nil {
		return fmt.Errorf("Items field should not be set to nil")
	}

	kubeControllersConfigServer, err := kubeControllersConfigClient.Create(ctx, kubeControllersConfig, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating the kubeControllersConfig '%v' (%v)", kubeControllersConfig, err)
	}
	if kubeControllersConfigServer.Name != "default" {
		return fmt.Errorf("didn't get the same kubeControllersConfig back from the server \n%+v\n%+v", kubeControllersConfig, kubeControllersConfigServer)
	}
	if !reflect.DeepEqual(kubeControllersConfigServer.Status, v3.KubeControllersConfigurationStatus{}) {
		return fmt.Errorf("status was set on create to %#v", kubeControllersConfigServer.Status)
	}

	kubeControllersConfigs, err = kubeControllersConfigClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing kubeControllersConfigs (%s)", err)
	}
	if len(kubeControllersConfigs.Items) != 1 {
		return fmt.Errorf("expected 1 kubeControllersConfig got %d", len(kubeControllersConfigs.Items))
	}

	kubeControllersConfigServer, err = kubeControllersConfigClient.Get(ctx, "default", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting kubeControllersConfig default (%s)", err)
	}
	if kubeControllersConfigServer.Name != "default" &&
		kubeControllersConfig.ResourceVersion == kubeControllersConfigServer.ResourceVersion {
		return fmt.Errorf("didn't get the same kubeControllersConfig back from the server \n%+v\n%+v", kubeControllersConfig, kubeControllersConfigServer)
	}

	kubeControllersConfigUpdate := kubeControllersConfigServer.DeepCopy()
	kubeControllersConfigUpdate.Spec.HealthChecks = v3.Enabled
	kubeControllersConfigUpdate.Status.EnvironmentVars = map[string]string{"FOO": "bar"}
	kubeControllersConfigServer, err = kubeControllersConfigClient.Update(ctx, kubeControllersConfigUpdate, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("error updating kubeControllersConfig default (%s)", err)
	}
	if kubeControllersConfigServer.Spec.HealthChecks != kubeControllersConfigUpdate.Spec.HealthChecks {
		return errors.New("didn't update spec.content")
	}
	if kubeControllersConfigServer.Status.EnvironmentVars != nil {
		return errors.New("status was updated by Update()")
	}

	kubeControllersConfigUpdate = kubeControllersConfigServer.DeepCopy()
	kubeControllersConfigUpdate.Status.EnvironmentVars = map[string]string{"FIZZ": "buzz"}
	kubeControllersConfigUpdate.Labels = map[string]string{"foo": "bar"}
	kubeControllersConfigUpdate.Spec.HealthChecks = ""
	kubeControllersConfigServer, err = kubeControllersConfigClient.UpdateStatus(ctx, kubeControllersConfigUpdate, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("error updating kubeControllersConfig default (%s)", err)
	}
	if !reflect.DeepEqual(kubeControllersConfigServer.Status, kubeControllersConfigUpdate.Status) {
		return fmt.Errorf("didn't update status. %v != %v", kubeControllersConfigUpdate.Status, kubeControllersConfigServer.Status)
	}
	if _, ok := kubeControllersConfigServer.Labels["foo"]; ok {
		return fmt.Errorf("updatestatus updated labels")
	}
	if kubeControllersConfigServer.Spec.HealthChecks == "" {
		return fmt.Errorf("updatestatus updated spec")
	}

	err = kubeControllersConfigClient.Delete(ctx, "default", metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("kubeControllersConfig should be deleted (%s)", err)
	}

	// Test watch
	w, err := client.ProjectcalicoV3().KubeControllersConfigurations().Watch(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error watching KubeControllersConfigurations (%s)", err)
	}
	var events []watch.Event
	done := sync.WaitGroup{}
	done.Add(1)
	timeout := time.After(500 * time.Millisecond)
	var timeoutErr error
	// watch for 2 events
	go func() {
		defer done.Done()
		for i := 0; i < 2; i++ {
			select {
			case e := <-w.ResultChan():
				events = append(events, e)
			case <-timeout:
				timeoutErr = fmt.Errorf("timed out waiting for events")
				return
			}
		}
	}()

	// Create, then delete KubeControllersConfigurations
	_, err = kubeControllersConfigClient.Create(ctx, kubeControllersConfig, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating the kubeControllersConfig '%v' (%v)", kubeControllersConfig, err)
	}
	err = kubeControllersConfigClient.Delete(ctx, "default", metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("kubeControllersConfig should be deleted (%s)", err)
	}

	done.Wait()
	if timeoutErr != nil {
		return timeoutErr
	}
	if len(events) != 2 {
		return fmt.Errorf("expected 2 watch events got %d", len(events))
	}

	return nil
}

// TestClusterInformationClient exercises the ClusterInformation client.
func TestClusterInformationClient(t *testing.T) {
	const name = "default"
	rootTestFunc := func() func(t *testing.T) {
		return func(t *testing.T) {
			client, shutdownServer := getFreshApiserverAndClient(t, func() runtime.Object {
				return &v3.ClusterInformation{}
			})
			defer shutdownServer()
			if err := testClusterInformationClient(client, name); err != nil {
				t.Fatal(err)
			}
		}
	}

	if !t.Run(name, rootTestFunc()) {
		t.Errorf("test-clusterinformation test failed")
	}
}

func testClusterInformationClient(client calicoclient.Interface, name string) error {
	clusterInformationClient := client.ProjectcalicoV3().ClusterInformations()
	ctx := context.Background()

	ci, err := clusterInformationClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing ClusterInformation (%s)", err)
	}
	if ci.Items == nil {
		return fmt.Errorf("items field should not be set to nil")
	}

	// Confirm it's not possible to edit the default cluster information.
	info := ci.Items[0]
	info.Spec.CalicoVersion = "fakeVersion"
	_, err = clusterInformationClient.Update(ctx, &info, metav1.UpdateOptions{})
	if err == nil {
		return fmt.Errorf("expected error updating default clusterinformation")
	}

	// Should also not be able to delete it.
	err = clusterInformationClient.Delete(ctx, "default", metav1.DeleteOptions{})
	if err == nil {
		return fmt.Errorf("expected error updating default clusterinformation")
	}

	// Confirm it's not possible to create a clusterInformation obj with name other than "default"
	invalidClusterInfo := &v3.ClusterInformation{ObjectMeta: metav1.ObjectMeta{Name: "test-clusterinformation"}}

	_, err = clusterInformationClient.Create(ctx, invalidClusterInfo, metav1.CreateOptions{})
	if err == nil {
		return fmt.Errorf("expected error creating invalidClusterInfo with name other than \"default\"")
	}

	return nil
}

// TestCalicoNodeStatusClient exercises the CalicoNodeStatus client.
func TestCalicoNodeStatusClient(t *testing.T) {
	const name = "test-caliconodestatus"
	rootTestFunc := func() func(t *testing.T) {
		return func(t *testing.T) {
			client, shutdownServer := getFreshApiserverAndClient(t, func() runtime.Object {
				return &v3.CalicoNodeStatus{}
			})
			defer shutdownServer()
			if err := testCalicoNodeStatusClient(client, name); err != nil {
				t.Fatal(err)
			}
		}
	}

	if !t.Run(name, rootTestFunc()) {
		t.Errorf("test-caliconodestatus test failed")
	}
}

func testCalicoNodeStatusClient(client calicoclient.Interface, name string) error {
	seconds := uint32(11)
	caliconodestatusClient := client.ProjectcalicoV3().CalicoNodeStatuses()
	caliconodestatus := &v3.CalicoNodeStatus{
		ObjectMeta: metav1.ObjectMeta{Name: name},

		Spec: v3.CalicoNodeStatusSpec{
			Node: "node1",
			Classes: []v3.NodeStatusClassType{
				v3.NodeStatusClassTypeAgent,
				v3.NodeStatusClassTypeBGP,
				v3.NodeStatusClassTypeRoutes,
			},
			UpdatePeriodSeconds: &seconds,
		},
	}
	ctx := context.Background()

	// start from scratch
	caliconodestatuses, err := caliconodestatusClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing caliconodestatuses (%s)", err)
	}
	if caliconodestatuses.Items == nil {
		return fmt.Errorf("items field should not be set to nil")
	}

	caliconodestatusNew, err := caliconodestatusClient.Create(ctx, caliconodestatus, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating the object '%v' (%v)", caliconodestatus, err)
	}
	if name != caliconodestatusNew.Name {
		return fmt.Errorf("didn't get the same object back from the server \n%+v\n%+v", caliconodestatus, caliconodestatusNew)
	}

	_, err = caliconodestatusClient.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting object %s (%s)", name, err)
	}

	err = caliconodestatusClient.Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("object should be deleted (%s)", err)
	}

	return nil
}

// TestIPAMConfigClient exercises the IPAMConfig client.
func TestIPAMConfigClient(t *testing.T) {
	const name = "test-ipamconfig"
	rootTestFunc := func() func(t *testing.T) {
		return func(t *testing.T) {
			client, shutdownServer := getFreshApiserverAndClient(t, func() runtime.Object {
				return &v3.IPAMConfiguration{}
			})
			defer shutdownServer()
			if err := testIPAMConfigClient(client, name); err != nil {
				t.Fatal(err)
			}
		}
	}

	if !t.Run(name, rootTestFunc()) {
		t.Errorf("test-ipamconfig test failed")
	}
}

func testIPAMConfigClient(client calicoclient.Interface, name string) error {
	ipamConfigClient := client.ProjectcalicoV3().IPAMConfigurations()
	ipamConfig := &v3.IPAMConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: name},

		Spec: v3.IPAMConfigurationSpec{
			StrictAffinity:   true,
			MaxBlocksPerHost: 28,
		},
	}
	ctx := context.Background()

	_, err := ipamConfigClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing IPAMConfigurations: %s", err)
	}

	_, err = ipamConfigClient.Create(ctx, ipamConfig, metav1.CreateOptions{})
	if err == nil {
		return fmt.Errorf("should not be able to create ipam config %s ", ipamConfig.Name)
	}

	ipamConfig.Name = "default"
	ipamConfigNew, err := ipamConfigClient.Create(ctx, ipamConfig, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating the object '%v' (%v)", ipamConfig, err)
	}

	if ipamConfigNew.Name != ipamConfig.Name {
		return fmt.Errorf("didn't get the same object back from the server \n%+v\n%+v", ipamConfig, ipamConfigNew)
	}

	if ipamConfigNew.Spec.StrictAffinity != true || ipamConfig.Spec.MaxBlocksPerHost != 28 {
		return fmt.Errorf("didn't get the correct object back from the server \n%+v\n%+v", ipamConfig, ipamConfigNew)
	}

	ipamConfigNew, err = ipamConfigClient.Get(ctx, ipamConfig.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting object %s (%s)", ipamConfig.Name, err)
	}

	ipamConfigNew.Spec.StrictAffinity = false
	ipamConfigNew.Spec.MaxBlocksPerHost = 0

	_, err = ipamConfigClient.Update(ctx, ipamConfigNew, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("error updating object %s (%s)", name, err)
	}

	ipamConfigUpdated, err := ipamConfigClient.Get(ctx, ipamConfig.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting object %s (%s)", ipamConfig.Name, err)
	}

	if ipamConfigUpdated.Spec.StrictAffinity != false || ipamConfigUpdated.Spec.MaxBlocksPerHost != 0 {
		return fmt.Errorf("didn't get the correct object back from the server \n%+v\n%+v", ipamConfigUpdated, ipamConfigNew)
	}

	err = ipamConfigClient.Delete(ctx, ipamConfig.Name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("object should be deleted (%s)", err)
	}

	return nil
}

// TestBlockAffinityClient exercises the BlockAffinity client.
func TestBlockAffinityClient(t *testing.T) {
	const name = "test-blockaffinity"
	rootTestFunc := func() func(t *testing.T) {
		return func(t *testing.T) {
			client, shutdownServer := getFreshApiserverAndClient(t, func() runtime.Object {
				return &v3.BlockAffinity{}
			})
			defer shutdownServer()
			if err := testBlockAffinityClient(client, name); err != nil {
				t.Fatal(err)
			}
		}
	}

	if !t.Run(name, rootTestFunc()) {
		t.Errorf("test-blockaffinity test failed")
	}
}

func testBlockAffinityClient(client calicoclient.Interface, name string) error {
	blockAffinityClient := client.ProjectcalicoV3().BlockAffinities()
	blockAffinity := &v3.BlockAffinity{
		ObjectMeta: metav1.ObjectMeta{Name: name},

		Spec: v3.BlockAffinitySpec{
			CIDR:  "10.0.0.0/24",
			Node:  "node1",
			State: "pending",
		},
	}
	libV3BlockAffinity := &libapiv3.BlockAffinity{
		ObjectMeta: metav1.ObjectMeta{Name: name},

		Spec: libapiv3.BlockAffinitySpec{
			CIDR:    "10.0.0.0/24",
			Node:    "node1",
			State:   "pending",
			Deleted: "false",
		},
	}
	ctx := context.Background()

	// Calico libv3 client instantiation in order to get around the API create restrictions
	// TODO: Currently these tests only run on a Kubernetes datastore since profile creation
	// does not work in etcd. Figure out how to divide this configuration to etcd once that
	// is fixed.
	config := apiconfig.NewCalicoAPIConfig()
	config.Spec = apiconfig.CalicoAPIConfigSpec{
		DatastoreType: apiconfig.Kubernetes,
		EtcdConfig: apiconfig.EtcdConfig{
			EtcdEndpoints: "http://localhost:2379",
		},
		KubeConfig: apiconfig.KubeConfig{
			Kubeconfig: os.Getenv("KUBECONFIG"),
		},
	}
	apiClient, err := libclient.New(*config)
	if err != nil {
		return fmt.Errorf("unable to create Calico lib v3 client: %s", err)
	}

	_, err = blockAffinityClient.Create(ctx, blockAffinity, metav1.CreateOptions{})
	if err == nil {
		return fmt.Errorf("should not be able to create block affinity %s ", blockAffinity.Name)
	}

	// Create the block affinity using the libv3 client.
	_, err = apiClient.BlockAffinities().Create(ctx, libV3BlockAffinity, options.SetOptions{})
	if err != nil {
		return fmt.Errorf("error creating the object through the Calico v3 API '%v' (%v)", libV3BlockAffinity, err)
	}

	blockAffinityNew, err := blockAffinityClient.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting object %s (%s)", name, err)
	}

	blockAffinityList, err := blockAffinityClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing BlockAffinity (%s)", err)
	}
	if blockAffinityList.Items == nil {
		return fmt.Errorf("items field should not be set to nil")
	}

	blockAffinityNew.Spec.State = "confirmed"

	_, err = blockAffinityClient.Update(ctx, blockAffinityNew, metav1.UpdateOptions{})
	if err == nil {
		return fmt.Errorf("should not be able to update block affinity %s", blockAffinityNew.Name)
	}

	err = blockAffinityClient.Delete(ctx, name, metav1.DeleteOptions{})
	if nil == err {
		return fmt.Errorf("should not be able to delete block affinity %s", blockAffinity.Name)
	}

	// Test watch
	w, err := blockAffinityClient.Watch(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error watching block affinities (%s)", err)
	}

	_, err = apiClient.BlockAffinities().Delete(ctx, name, options.DeleteOptions{ResourceVersion: blockAffinityNew.ResourceVersion})
	if err != nil {
		return fmt.Errorf("error deleting the object through the Calico v3 API '%v' (%v)", name, err)
	}

	// Verify watch
	var events []watch.Event
	timeout := time.After(500 * time.Millisecond)
	var timeoutErr error
	// watch for 2 events
	for i := 0; i < 2; i++ {
		select {
		case e := <-w.ResultChan():
			events = append(events, e)
		case <-timeout:
			timeoutErr = fmt.Errorf("timed out waiting for events")
			break
		}
	}
	if timeoutErr != nil {
		return timeoutErr
	}
	if len(events) != 2 {
		return fmt.Errorf("expected 2 watch events got %d", len(events))
	}

	return nil
}

// TestBGPFilterClient exercises the BGPFilter client.
func TestBGPFilterClient(t *testing.T) {
	const name = "test-bgpfilter"
	rootTestFunc := func() func(t *testing.T) {
		return func(t *testing.T) {
			client, shutdownServer := getFreshApiserverAndClient(t, func() runtime.Object {
				return &v3.BGPFilter{}
			})
			defer shutdownServer()
			if err := testBGPFilterClient(client, name); err != nil {
				t.Fatal(err)
			}
		}
	}

	if !t.Run(name, rootTestFunc()) {
		t.Errorf("test-bgpfilter test failed")
	}
}

func testBGPFilterClient(client calicoclient.Interface, name string) error {
	bgpFilterClient := client.ProjectcalicoV3().BGPFilters()
	r1v4 := v3.BGPFilterRuleV4{
		CIDR:          "10.10.10.0/24",
		MatchOperator: v3.In,
		Source:        v3.BGPFilterSourceRemotePeers,
		Interface:     "*.calico",
		Action:        v3.Accept,
	}
	r1v6 := v3.BGPFilterRuleV6{
		CIDR:          "dead:beef:1::/64",
		MatchOperator: v3.Equal,
		Source:        v3.BGPFilterSourceRemotePeers,
		Interface:     "*.calico",
		Action:        v3.Accept,
	}
	r2v4 := v3.BGPFilterRuleV4{
		CIDR:          "10.10.10.0/24",
		MatchOperator: v3.In,
		Source:        v3.BGPFilterSourceRemotePeers,
		Action:        v3.Accept,
	}
	r2v6 := v3.BGPFilterRuleV6{
		CIDR:          "dead:beef:1::/64",
		MatchOperator: v3.Equal,
		Source:        v3.BGPFilterSourceRemotePeers,
		Action:        v3.Accept,
	}
	r3v4 := v3.BGPFilterRuleV4{
		CIDR:          "10.10.10.0/24",
		MatchOperator: v3.In,
		Interface:     "*.calico",
		Action:        v3.Accept,
	}
	r3v6 := v3.BGPFilterRuleV6{
		CIDR:          "dead:beef:1::/64",
		MatchOperator: v3.Equal,
		Interface:     "*.calico",
		Action:        v3.Accept,
	}
	r4v4 := v3.BGPFilterRuleV4{
		Source:    v3.BGPFilterSourceRemotePeers,
		Interface: "*.calico",
		Action:    v3.Accept,
	}
	r4v6 := v3.BGPFilterRuleV6{
		Source:    v3.BGPFilterSourceRemotePeers,
		Interface: "*.calico",
		Action:    v3.Accept,
	}
	r5v4 := v3.BGPFilterRuleV4{
		CIDR:          "10.10.10.0/24",
		MatchOperator: v3.In,
		Source:        v3.BGPFilterSourceRemotePeers,
		Action:        v3.Accept,
	}
	r5v6 := v3.BGPFilterRuleV6{
		CIDR:          "dead:beef:1::/64",
		MatchOperator: v3.Equal,
		Action:        v3.Accept,
	}
	r6v4 := v3.BGPFilterRuleV4{
		Source: v3.BGPFilterSourceRemotePeers,
		Action: v3.Accept,
	}
	r6v6 := v3.BGPFilterRuleV6{
		Source: v3.BGPFilterSourceRemotePeers,
		Action: v3.Accept,
	}
	r7v4 := v3.BGPFilterRuleV4{
		Interface: "*.calico",
		Action:    v3.Accept,
	}
	r7v6 := v3.BGPFilterRuleV6{
		Interface: "*.calico",
		Action:    v3.Accept,
	}
	r8v4 := v3.BGPFilterRuleV4{
		Action: v3.Accept,
	}
	r8v6 := v3.BGPFilterRuleV6{
		Action: v3.Accept,
	}

	// This test expect equal number of rules in each of ExportV4, ImportV4, ExportV6 and ImportV6.
	bgpFilter := &v3.BGPFilter{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v3.BGPFilterSpec{
			ExportV4: []v3.BGPFilterRuleV4{r1v4, r7v4, r6v4, r5v4, r2v4, r8v4},
			ImportV4: []v3.BGPFilterRuleV4{r2v4, r3v4, r4v4, r7v4, r8v4, r1v4},
			ExportV6: []v3.BGPFilterRuleV6{r5v6, r1v6, r6v6, r4v6, r8v6, r2v6},
			ImportV6: []v3.BGPFilterRuleV6{r6v6, r1v6, r3v6, r7v6, r2v6, r4v6},
		},
	}
	ctx := context.Background()

	_, err := bgpFilterClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing BGPFilters: %s", err)
	}

	bgpFilterNew, err := bgpFilterClient.Create(ctx, bgpFilter, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating the object '%v' (%v)", bgpFilter, err)
	}

	if bgpFilterNew.Name != bgpFilter.Name {
		return fmt.Errorf("didn't get the same object back from the server \n%+v\n%+v", bgpFilter, bgpFilterNew)
	}

	size := len(bgpFilter.Spec.ExportV4)
	if len(bgpFilterNew.Spec.ExportV4) != size || len(bgpFilterNew.Spec.ImportV4) != size ||
		len(bgpFilterNew.Spec.ExportV6) != size || len(bgpFilterNew.Spec.ImportV6) != size {
		return fmt.Errorf("didn't get the correct object back from the server \n%+v\n%+v", bgpFilter, bgpFilterNew)
	}

	for i := 0; i < size; i++ {
		if bgpFilterNew.Spec.ExportV4[i] != bgpFilter.Spec.ExportV4[i] {
			return fmt.Errorf("didn't get the correct object back from the server. Incorrect ExportV4: \n%+v\n%+v",
				bgpFilter.Spec.ExportV4, bgpFilterNew.Spec.ExportV4)
		}
		if bgpFilterNew.Spec.ImportV4[i] != bgpFilter.Spec.ImportV4[i] {
			return fmt.Errorf("didn't get the correct object back from the server. Incorrect ImportV4: \n%+v\n%+v",
				bgpFilter.Spec.ImportV4, bgpFilterNew.Spec.ImportV4)
		}
		if bgpFilterNew.Spec.ExportV6[i] != bgpFilter.Spec.ExportV6[i] {
			return fmt.Errorf("didn't get the correct object back from the server. Incorrect ExportV6: \n%+v\n%+v",
				bgpFilter.Spec.ExportV6, bgpFilterNew.Spec.ExportV6)
		}
		if bgpFilterNew.Spec.ImportV6[i] != bgpFilter.Spec.ImportV6[i] {
			return fmt.Errorf("didn't get the correct object back from the server. Incorrect ImportV6: \n%+v\n%+v",
				bgpFilter.Spec.ImportV6, bgpFilterNew.Spec.ImportV6)
		}
	}

	bgpFilterNew, err = bgpFilterClient.Get(ctx, bgpFilter.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting object %s (%s)", bgpFilter.Name, err)
	}

	bgpFilterNew.Spec.ExportV4 = nil

	_, err = bgpFilterClient.Update(ctx, bgpFilterNew, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("error updating object %s (%s)", name, err)
	}

	bgpFilterUpdated, err := bgpFilterClient.Get(ctx, bgpFilter.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting object %s (%s)", bgpFilter.Name, err)
	}

	if bgpFilterUpdated.Spec.ExportV4 != nil {
		return fmt.Errorf("didn't get the correct object back from the server \n%+v\n%+v", bgpFilterUpdated, bgpFilterNew)
	}

	err = bgpFilterClient.Delete(ctx, bgpFilter.Name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("object should be deleted (%s)", err)
	}

	return nil
}

// TestPolicyWatch checks that the WatchManager closes watch when a new Tier is added
func TestPolicyWatch(t *testing.T) {
	const name = "test-policywatch"
	rootTestFunc := func() func(t *testing.T) {
		return func(t *testing.T) {
			client, shutdownServer := getFreshApiserverAndClient(t, func() runtime.Object {
				return &v3.GlobalNetworkPolicy{}
			})
			defer shutdownServer()
			if err := testPolicyWatch(client); err != nil {
				t.Fatal(err)
			}
		}
	}

	if !t.Run(name, rootTestFunc()) {
		t.Errorf("test-policywatch test failed")
	}
}

func testPolicyWatch(client calicoclient.Interface) error {
	globalNetworkPolicy := &v3.GlobalNetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "global-net-pol-watch"}}
	_, err := client.ProjectcalicoV3().GlobalNetworkPolicies().Create(context.Background(), globalNetworkPolicy, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating GlobalNetworkPolicy (%s)", err)
	}
	defer func() {
		_ = client.ProjectcalicoV3().GlobalNetworkPolicies().Delete(context.Background(), globalNetworkPolicy.Name, metav1.DeleteOptions{})
	}()

	w, err := client.ProjectcalicoV3().GlobalNetworkPolicies().Watch(context.Background(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error watching GlobalNetworkPolicy (%s)", err)
	}

	done := sync.WaitGroup{}
	done.Add(1)
	timeout := time.After(5 * time.Second)
	var timeoutErr error
	var event watch.Event

	go func() {
		defer done.Done()
		for {
			select {
			case event = <-w.ResultChan():
				return
			case <-timeout:
				timeoutErr = fmt.Errorf("timed out waiting for events")
				return
			}
		}
	}()

	done.Wait()

	if timeoutErr != nil {
		return timeoutErr
	}

	if event.Type != watch.Added {
		return fmt.Errorf("unexpected event type %s", event)
	}

	order := float64(100.0)
	tier := &v3.Tier{
		ObjectMeta: metav1.ObjectMeta{Name: "custom-tier"},
		Spec: v3.TierSpec{
			Order: &order,
		},
	}

	// Creating a new Tier should force the watch to be closed
	_, err = client.ProjectcalicoV3().Tiers().Create(context.Background(), tier, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating Tier (%s)", err)
	}
	defer func() {
		_ = client.ProjectcalicoV3().Tiers().Delete(context.Background(), tier.Name, metav1.DeleteOptions{})
	}()

	done = sync.WaitGroup{}
	done.Add(1)
	var chClosedError error
	timeout = time.After(5 * time.Second)

	go func() {
		defer done.Done()
		for {
			select {
			case _, ok := <-w.ResultChan():
				if !ok {
					return
				}
			case <-timeout:
				chClosedError = fmt.Errorf("watch should be closed")
				return
			}
		}
	}()

	done.Wait()

	if chClosedError != nil {
		return chClosedError
	}

	return nil
}
