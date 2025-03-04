//go:build fvtests

// Copyright (c) 2017-2019,2021 Tigera, Inc. All rights reserved.

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
// limitations under the License.

package fv_test

// The tests in this file test Felix's and Typha's health endpoints, http://.../liveness and
// http://.../readiness.
//
// Felix should report itself as live, so long as its calc_graph and int_dataplane loops have not
// died or hung; and as ready, so long as it has completed its initial dataplane programming, is
// connected to its datastore, and is not doing a resync (either the initial resync, or a subsequent
// one).
//
// Typha should report itself as live, so long as its Felix-serving loop has not died or hung; and
// as ready, so long as it is connected to its datastore, and is not doing a resync (either the
// initial resync, or a subsequent one).
//
// (These reports are useful because k8s can detect and handle a pod that is consistently non-live,
// by killing and restarting it; and can adjust for a pod that is non-ready, by (a) not routing
// Service traffic to it (when that pod is otherwise one of the possible backends for a Service),
// and (b) not moving on to the next pod, in a rolling upgrade process, until the just-upgraded pod
// says that it is ready.)

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/types"
	v3 "github.com/projectcalico/api/pkg/apis/projectcalico/v3"
	log "github.com/sirupsen/logrus"

	"github.com/projectcalico/calico/felix/fv/containers"
	"github.com/projectcalico/calico/felix/fv/infrastructure"
	"github.com/projectcalico/calico/felix/fv/utils"
	"github.com/projectcalico/calico/felix/fv/workload"
	"github.com/projectcalico/calico/libcalico-go/lib/health"
	"github.com/projectcalico/calico/libcalico-go/lib/net"
	"github.com/projectcalico/calico/libcalico-go/lib/options"
)

var _ = Describe("_HEALTH_ _BPF-SAFE_ health tests", func() {
	var k8sInfra *infrastructure.K8sDatastoreInfra
	var felix *infrastructure.Felix

	felixReady := func() int {
		return healthStatus(felix.IP, "9099", "readiness")
	}

	felixLiveness := func() int {
		return healthStatus(felix.IP, "9099", "liveness")
	}

	BeforeEach(func() {
		var err error
		k8sInfra, err = infrastructure.GetK8sDatastoreInfra(infrastructure.K8SInfraLocalCluster)
		Expect(err).NotTo(HaveOccurred())

		// Avoid cross-talk between tests.
		felix = nil
	})

	AfterEach(func() {
		felix.Stop()
		k8sInfra.Stop()
	})

	// describeCommonFelixTests creates specs for Felix tests that are common between the
	// two scenarios below (with and without Typha).
	describeCommonFelixTests := func() {
		BeforeEach(func() {
			waitForMainLoop(felix)
		})

		Describe("with normal Felix startup", func() {
			It("should become ready and stay ready", func() {
				Eventually(felixReady, "5s", "100ms").Should(BeGood())
				Consistently(felixReady, "10s", "1s").Should(BeGood())
			})

			It("should become live and stay live", func() {
				Eventually(felixLiveness, "5s", "100ms").Should(BeGood())
				Consistently(felixLiveness, "10s", "1s").Should(BeGood())
			})
		})

		createLocalPod := func() {
			testPodName := fmt.Sprintf("test-pod-%x", rand.Uint32())
			podIP := "10.0.0.1"
			pod := workload.New(felix, testPodName, "default",
				podIP, "12345", "tcp")
			pod.Start()

			pod.ConfigureInInfra(k8sInfra)
		}

		Describe("after removing iptables-restore/nft", func() {
			BeforeEach(func() {
				// Wait until felix gets into steady state.
				Eventually(felixReady, "5s", "100ms").Should(BeGood())

				bin := "/usr/sbin/iptables-legacy-restore"
				if NFTMode() {
					bin = "/usr/sbin/nft"
				}

				// Then remove iptables-restore.
				err := felix.ExecMayFail("rm", bin)
				Expect(err).NotTo(HaveOccurred())

				// Make an update that will force felix to run iptables-restore.
				createLocalPod()
			})

			It("should become unready, then die", func() {
				Eventually(felixReady, "120s", "10s").ShouldNot(BeGood())
				Eventually(felix.Stopped, "5s").Should(BeTrue())
			})
		})

		Describe("after replacing iptables/nftables with a slow version", func() {
			BeforeEach(func() {
				// Wait until felix gets into steady state.
				Eventually(felixReady, "5s", "100ms").Should(BeGood())

				toReplace := "/usr/sbin/iptables-legacy-restore"
				slowBinary := "slow-iptables-restore"
				if NFTMode() {
					toReplace = "/usr/sbin/nft"
					slowBinary = "slow-nft"
				}

				// Then replace iptables-restore with the bad version:

				// We need to delete the file first since it's a symlink and "docker cp"
				// follows the link and overwrites the wrong file if we don't.
				err := felix.ExecMayFail("rm", toReplace)
				Expect(err).NotTo(HaveOccurred())

				// Copy in the nobbled iptables command.
				err = felix.CopyFileIntoContainer(slowBinary, toReplace)
				Expect(err).NotTo(HaveOccurred())

				// Make it executable.
				err = felix.ExecMayFail("chmod", "+x", toReplace)
				Expect(err).NotTo(HaveOccurred())

				// Make an update that will force felix to run iptables-restore.
				createLocalPod()
			})

			It("should detect dataplane pause and become non-ready", func() {
				Eventually(felixReady, "120s", "10s").ShouldNot(BeGood())
			})
		})
	}

	var typhaContainer *containers.Container
	var typhaReady, typhaLiveness func() int

	startTypha := func(getDockerArgs func() []string) {
		typhaContainer = containers.Run("typha",
			containers.RunOpts{AutoRemove: true},
			append(getDockerArgs(),
				"--privileged",
				"-e", "TYPHA_HEALTHENABLED=true",
				"-e", "TYPHA_HEALTHHOST=0.0.0.0",
				"-e", "TYPHA_LOGSEVERITYSCREEN=info",
				"-e", "TYPHA_DATASTORETYPE=kubernetes",
				"-e", "TYPHA_PROMETHEUSMETRICSENABLED=true",
				"-e", "TYPHA_USAGEREPORTINGENABLED=false",
				"-e", "TYPHA_DEBUGMEMORYPROFILEPATH=\"heap-<timestamp>\"",
				utils.Config.TyphaImage,
				"calico-typha")...)
		Expect(typhaContainer).NotTo(BeNil())
		typhaReady = healthStatusFn(typhaContainer.IP, "9098", "readiness")
		typhaLiveness = healthStatusFn(typhaContainer.IP, "9098", "liveness")
	}

	type felixParams struct {
		dataplaneTimeout, calcGraphTimeout, calcGraphHangTime, dataplaneHangTime, healthHost string
	}
	startFelix := func(typhaAddr string, getDockerArgs func() []string, params felixParams) {
		envVars := map[string]string{
			"FELIX_HEALTHENABLED":                   "true",
			"FELIX_HEALTHHOST":                      params.healthHost,
			"FELIX_DEBUGMEMORYPROFILEPATH":          "heap-<timestamp>",
			"FELIX_DataplaneWatchdogTimeout":        params.dataplaneTimeout,
			"FELIX_DebugSimulateCalcGraphHangAfter": params.calcGraphHangTime,
			"FELIX_DebugSimulateDataplaneHangAfter": params.dataplaneHangTime,
			"FELIX_TYPHAADDR":                       typhaAddr,
		}
		if params.calcGraphTimeout != "" {
			envVars["FELIX_HealthTimeoutOverrides"] = "CalculationGraph=" + params.calcGraphTimeout
		}
		felix = infrastructure.RunFelix(
			k8sInfra, 0, infrastructure.TopologyOptions{
				EnableIPv6:      false,
				ExtraEnvVars:    envVars,
				DelayFelixStart: true,
			},
		)
		if BPFMode() {
			// In BPF mode, felix needs the Node to be configured.
			ipPoolCIDR := net.MustParseCIDR("10.70.0.0/24")
			k8sInfra.AddNode(felix, &ipPoolCIDR.IPNet, nil, 0, true)
		}
		felix.TriggerDelayedStart()
	}

	Describe("healthHost not 'all interfaces'", func() {
		checkHealthInternally := func() error {
			_, err := felix.ExecOutput("wget", "-S", "-T", "2", "http://127.0.0.1:9099/readiness", "-O", "-")
			return err
		}

		It("should run healthchecks on localhost by default", func() {
			startFelix("", k8sInfra.GetDockerArgs, felixParams{dataplaneTimeout: "20s"})
			Eventually(checkHealthInternally, "10s", "100ms").ShouldNot(HaveOccurred())
		})

		It("should run support running healthchecks on '127.0.0.1'", func() {
			startFelix("", k8sInfra.GetDockerArgs, felixParams{dataplaneTimeout: "20", healthHost: "127.0.0.1"})
			Eventually(checkHealthInternally, "10s", "100ms").ShouldNot(HaveOccurred())
		})

		It("should support running healthchecks on 'localhost'", func() {
			startFelix("", k8sInfra.GetDockerArgs, felixParams{dataplaneTimeout: "20", healthHost: "localhost"})
			Eventually(checkHealthInternally, "10s", "100ms").ShouldNot(HaveOccurred())
		})
	})

	Describe("with Felix running (no Typha)", func() {
		BeforeEach(func() {
			startFelix("", k8sInfra.GetDockerArgs, felixParams{dataplaneTimeout: "20", healthHost: "0.0.0.0"})
		})

		describeCommonFelixTests()
	})

	Describe("with Felix (no Typha) and Felix calc graph set to hang (10s calc graph timeout)", func() {
		BeforeEach(func() {
			startFelix("", k8sInfra.GetDockerArgs, felixParams{calcGraphTimeout: "10s", calcGraphHangTime: "5", healthHost: "0.0.0.0"})
			waitForMainLoop(felix)
		})

		It("should report live initially, then become non-live", func() {
			Eventually(felixLiveness, "10s", "100ms").Should(BeGood())
			Eventually(felixLiveness, "30s", "100ms").Should(BeBad())
			Consistently(felixLiveness, "10s", "100ms").Should(BeBad())
		})
	})

	Describe("with Felix (no Typha) and Felix dataplane set to hang (default 90s timeout)", func() {
		BeforeEach(func() {
			startFelix("", k8sInfra.GetDockerArgs, felixParams{dataplaneHangTime: "5", healthHost: "0.0.0.0"})
			waitForMainLoop(felix)
		})

		It("should report live initially, then become non-live", func() {
			Eventually(felixLiveness, "10s", "100ms").Should(BeGood())
			Consistently(felixLiveness, "60s", "1s").Should(BeGood())
			Eventually(felixLiveness, "60s", "1s").Should(BeBad())
			Consistently(felixLiveness, "10s", "1s").Should(BeBad())
		})
	})

	Describe("with Felix (no Typha) and Felix dataplane set to hang (20s timeout)", func() {
		BeforeEach(func() {
			startFelix("", k8sInfra.GetDockerArgs, felixParams{dataplaneTimeout: "20", dataplaneHangTime: "5", healthHost: "0.0.0.0"})
			waitForMainLoop(felix)
		})

		It("should report live initially, then become non-live", func() {
			Eventually(felixLiveness, "10s", "100ms").Should(BeGood())
			Eventually(felixLiveness, "30s", "100ms").Should(BeBad())
			Consistently(felixLiveness, "10s", "100ms").Should(BeBad())
		})
	})

	Describe("with Felix and Typha running", func() {
		BeforeEach(func() {
			startTypha(k8sInfra.GetDockerArgs)
			startFelix(typhaContainer.IP+":5473", k8sInfra.GetDockerArgs, felixParams{dataplaneTimeout: "20", healthHost: "0.0.0.0"})
		})

		AfterEach(func() {
			typhaContainer.Stop()
		})

		describeCommonFelixTests()

		It("typha should report ready", func() {
			Eventually(typhaReady, "5s", "100ms").Should(BeGood())
			Consistently(typhaReady, "10s", "1s").Should(BeGood())
		})

		It("typha should report live", func() {
			Eventually(typhaLiveness, "5s", "100ms").Should(BeGood())
			Consistently(typhaLiveness, "10s", "1s").Should(BeGood())
		})
	})

	Describe("with Felix unable to connect to Typha at first (20s timeout)", func() {
		BeforeEach(func() {
			// We have to start Typha first so we can pass its IP to Felix.
			startTypha(k8sInfra.GetDockerArgs)
			// Start felix with the wrong Typha port so it won't be able to connect initially.  Then, we'll add a
			// NAT rule to steer the traffic to the right port below.
			startFelix(typhaContainer.IP+":5474" /*wrong port!*/, k8sInfra.GetDockerArgs, felixParams{dataplaneTimeout: "20", healthHost: "0.0.0.0"})
		})

		AfterEach(func() {
			typhaContainer.Stop()
		})

		It("should report not ready until it connects to Typha, then report ready", func() {
			Eventually(felixReady, "5s", "100ms").Should(BeBad())
			Consistently(felixReady, "5s", "100ms").Should(BeBad())

			// Add a NAT rule to steer traffic from the port that Felix is using to the correct Typha port.
			felix.Exec("iptables", "-t", "nat", "-A", "OUTPUT", "-p", "tcp",
				"--destination", typhaContainer.IP, "--dport", "5474", "-j", "DNAT", "--to-destination", ":5473")

			Eventually(felixReady, "5s", "100ms").Should(BeGood())
		})
	})

	Describe("with typha connected to bad API endpoint", func() {
		BeforeEach(func() {
			startTypha(k8sInfra.GetBadEndpointDockerArgs)
		})

		It("typha should not report ready", func() {
			Consistently(typhaReady, "10s", "1s").ShouldNot(BeGood())
		})

		It("typha should not report live", func() {
			Consistently(typhaLiveness, "10s", "1s").ShouldNot(BeGood())
		})
	})

	Describe("with datastore not ready (20s timeout)", func() {
		var info *v3.ClusterInformation

		BeforeEach(func() {
			var err error
			info, err = k8sInfra.GetCalicoClient().ClusterInformation().Get(
				context.Background(),
				"default",
				options.GetOptions{},
			)
			Expect(err).NotTo(HaveOccurred())
			log.Infof("info = %#v", info)
			notReady := false
			info.Spec.DatastoreReady = &notReady
			info, err = k8sInfra.GetCalicoClient().ClusterInformation().Update(
				context.Background(),
				info,
				options.SetOptions{},
			)
			Expect(err).NotTo(HaveOccurred())
			startFelix("", k8sInfra.GetDockerArgs, felixParams{dataplaneTimeout: "20", healthHost: "0.0.0.0"})
		})

		AfterEach(func() {
			if info != nil {
				ready := true
				info.Spec.DatastoreReady = &ready
				var err error
				info, err = k8sInfra.GetCalicoClient().ClusterInformation().Update(
					context.Background(),
					info,
					options.SetOptions{},
				)
				Expect(err).NotTo(HaveOccurred())
			}
		})

		It("felix should report ready", func() {
			Eventually(felixReady, "5s", "100ms").Should(BeGood())
			Consistently(felixReady, "10s", "1s").Should(BeGood())
		})

		It("felix should report live", func() {
			Eventually(felixLiveness, "5s", "100ms").Should(BeGood())
			Consistently(felixLiveness, "10s", "1s").Should(BeGood())
		})
	})

	Describe("with Felix connected to bad typha port", func() {
		BeforeEach(func() {
			startTypha(k8sInfra.GetDockerArgs)
			startFelix(typhaContainer.IP+":5474", k8sInfra.GetDockerArgs, felixParams{dataplaneTimeout: "20", healthHost: "0.0.0.0"})
		})
		AfterEach(func() {
			typhaContainer.Stop()
		})
		It("should become unready, then die", func() {
			Eventually(felixReady, "5s", "1s").ShouldNot(BeGood())
			Consistently(felix.Stopped, "20s").Should(BeFalse()) // Should stay up for 20+s
			Eventually(felix.Stopped, "15s").Should(BeTrue())    // Should die at roughly 30s.
		})
	})
})

const statusErr = -1

func healthStatusFn(ip, port, endpoint string) func() int {
	return func() int {
		return healthStatus(ip, port, endpoint)
	}
}

func healthStatus(ip, port, endpoint string) int {
	resp, err := http.Get("http://" + ip + ":" + port + "/" + endpoint)
	if err != nil {
		log.WithError(err).WithField("resp", resp).Warn("HTTP GET failed")
		return statusErr
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	log.WithField("resp", resp).Infof("Health response %v:\n%v\n", resp.StatusCode, string(body))
	return resp.StatusCode
}

func waitForMainLoop(felix *infrastructure.Felix) {
	EventuallyWithOffset(1, func() string {
		resp, err := http.Get("http://" + felix.IP + ":9099/readiness")
		if err != nil {
			log.WithError(err).WithField("resp", resp).Warn("HTTP GET failed")
			return "<error>"
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}, "10s").Should(ContainSubstring("InternalDataplaneMainLoop"))
	By("Felix main loop started, InternalDataplaneMainLoop shown in health.")
}

func BeBad() types.GomegaMatcher {
	return BeNumerically("==", health.StatusBad)
}

func BeGood() types.GomegaMatcher {
	return BeNumerically("==", health.StatusGood)
}
