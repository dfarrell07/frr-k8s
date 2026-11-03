// SPDX-License-Identifier:Apache-2.0

package tests

import (
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/openshift-kni/k8sreporter"
	"go.universe.tf/e2etest/pkg/container"
	"go.universe.tf/e2etest/pkg/executor"
	"go.universe.tf/e2etest/pkg/frr"

	frrk8sv1beta1 "github.com/metallb/frr-k8s/api/v1beta1"
	"github.com/metallb/frrk8stests/pkg/config"
	"github.com/metallb/frrk8stests/pkg/dump"
	"github.com/metallb/frrk8stests/pkg/infra"
	"github.com/metallb/frrk8stests/pkg/k8s"
	"github.com/metallb/frrk8stests/pkg/k8sclient"
	"github.com/metallb/frrk8stests/pkg/routes"
	. "github.com/onsi/gomega"
	frrconfig "go.universe.tf/e2etest/pkg/frr/config"
	frrcontainer "go.universe.tf/e2etest/pkg/frr/container"
	"go.universe.tf/e2etest/pkg/ipfamily"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	clientset "k8s.io/client-go/kubernetes"
)

var _ = ginkgo.Describe("Establish BGP session with EnableGracefulRestart", func() {
	var (
		cs        clientset.Interface
		updater   *config.Updater
		reporter  *k8sreporter.KubernetesReporter
		nodes     []corev1.Node
		observers *grCIObservers
	)

	cleanup := func(u *config.Updater) error {
		for _, c := range infra.FRRContainers {
			if err := c.UpdateConfigFile(frrconfig.Empty); err != nil {
				return fmt.Errorf("clear config in the infra container failed: %w", err)
			}
		}
		if err := u.Clean(); err != nil {
			return fmt.Errorf("clear config in the API failed: %w", err)
		}
		return nil
	}

	ginkgo.BeforeEach(func() {
		var err error
		observers = nil

		reporter = dump.NewK8sReporter(k8s.FRRK8sNamespace)
		updater, err = config.NewUpdater()
		Expect(err).NotTo(HaveOccurred())

		err = cleanup(updater)
		Expect(err).NotTo(HaveOccurred(), "cleanup config in API and infra containers")

		cs = k8sclient.New()
		nodes, err = k8s.Nodes(cs)
		Expect(err).NotTo(HaveOccurred())

	})

	ginkgo.AfterEach(func() {
		var diagnosticErr error
		defer func() {
			diagnosticErr = errors.Join(diagnosticErr, observers.cleanup(), cleanup(updater))
			Expect(diagnosticErr).NotTo(HaveOccurred(), "GR CI diagnostics and cleanup")
		}()
		// Finish the independent window before diagnostic FRR reads or cleanup,
		// including when the original Consistently assertion failed early.
		diagnosticErr = observers.finish()
		testName := ginkgo.CurrentSpecReport().LeafNodeText
		dump.K8sInfo(testName, reporter)
		dump.BGPInfo(testName, infra.FRRContainers, cs)
		if observers != nil {
			diagnosticErr = errors.Join(diagnosticErr, dumpGRCIRuntime(cs, observers.dir, "after", observers.expectedAFI, observers.earliestStart()))
		}
	})

	ginkgo.Context("When restarting the frrk8s deamon pods", func() {
		ginkgo.DescribeTable("both routes on nodes and on the external peers are maintained", func(ipFam ipfamily.Family, prefix string) {
			externalPrefixes := []string{}
			frrs := config.ContainersForVRF(infra.FRRContainers, "")
			for i, c := range frrs {
				l := fmt.Sprintf("200.200.%d.0/24", i)
				modify := func(frr *frrcontainer.FRR) {
					frr.NeighborConfig.ToAdvertiseV4 = []string{l}
				}

				if ipFam == ipfamily.IPv6 {
					l = fmt.Sprintf("2001:db%d::/64", i)
					modify = func(frr *frrcontainer.FRR) {
						frr.NeighborConfig.ToAdvertiseV6 = []string{l}
					}
				}

				err := frrcontainer.PairWithNodes(cs, c, ipFam, modify)
				Expect(err).NotTo(HaveOccurred(), "set frr config in infra containers failed")

				// Helpers must finish startup before the DUT-only restart. Re-arm
				// their startup timers; the DUT's 120s retention stays unchanged.
				out, err := c.Exec("vtysh", "-c", "configure terminal",
					"-c", fmt.Sprintf("router bgp %d", c.RouterConfig.ASN),
					"-c", "bgp graceful-restart restart-time 1",
					"-c", "bgp shutdown", "-c", "no bgp shutdown")
				Expect(err).NotTo(HaveOccurred(), "prepare helper %s: %s", c.Name, out)
				externalPrefixes = append(externalPrefixes, l)
			}

			peersConfig := config.PeersForContainers(frrs, ipFam,
				config.EnableAllowAll, config.EnableReceiveAllowAll, config.EnableGracefulRestart)

			frrConfigCR := frrk8sv1beta1.FRRConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "graceful-restart-test",
					Namespace: k8s.FRRK8sNamespace,
				},
				Spec: frrk8sv1beta1.FRRConfigurationSpec{
					BGP: frrk8sv1beta1.BGPConfig{
						Routers: []frrk8sv1beta1.Router{
							{
								ASN:       infra.FRRK8sASN,
								Neighbors: config.NeighborsFromPeers(peersConfig.PeersV4, peersConfig.PeersV6),
								Prefixes:  []string{prefix},
							},
						},
					},
				},
			}

			err := updater.Update(peersConfig.Secrets, frrConfigCR)
			Expect(err).NotTo(HaveOccurred(), "apply the CR in k8s api failed")

			Eventually(func() error {
				return ensureGRHelpersReady(cs, nodes, peersConfig.Peers())
			}, time.Minute, time.Second).ShouldNot(HaveOccurred(),
				"helpers should advertise a fresh OPEN without the restart bit")

			check := func() error {
				for _, p := range peersConfig.Peers() {
					if err := routes.CheckNeighborHasPrefix(p.FRR, p.FRR.RouterConfig.VRF, prefix, nodes); err != nil {
						return fmt.Errorf("Neigh %s does not have prefix %s: %w", p.FRR.Name, prefix, err)
					}
				}
				for _, n := range nodes {
					if err := checkPrefixesOnNode(n.Name, externalPrefixes); err != nil {
						return err
					}
				}
				return nil
			}

			Eventually(check, time.Minute, time.Second).ShouldNot(HaveOccurred(),
				"routes should exist before we restart frr-k8s")

			observers, err = startGRCIObservers(cs, nodes, externalPrefixes, frrs, prefix)
			Expect(err).NotTo(HaveOccurred(), "start independent kernel-route observers")

			ginkgo.By("GR started")
			c := make(chan struct{})
			var restartCompleted time.Time
			go func() { // go restart frr-k8s while Consistently check that route exists
				defer ginkgo.GinkgoRecover()
				err := k8s.RestartFRRK8sPods(cs)
				Expect(err).NotTo(HaveOccurred(), "frr-k8s pods failed to restart")
				restartCompleted = time.Now()
				close(c)
			}()

			// 2*time.Minute is important because that is the Graceful Restart timer.
			Consistently(check, 2*time.Minute, time.Second).ShouldNot(HaveOccurred())
			Eventually(c, time.Minute, time.Second).Should(BeClosed(), "restart FRRK8s pods are not yet ready")
			<-c // The close publishes restartCompleted to this goroutine.
			Expect(observers.checkRestartCoverage(restartCompleted)).NotTo(HaveOccurred(), "GR observer restart coverage")
		},
			ginkgo.Entry("IPV4", ipfamily.IPv4, "192.168.2.0/24"),
			ginkgo.Entry("IPV6", ipfamily.IPv6, "fc00:f853:ccd:e799::/64"),
		)
	})
})

// ensureGRHelpersReady checks the helper OPENs received by every node. An old
// session retains its R-bit after the helper startup timer expires, so reconnect
// those helpers during setup, before the continuous route-retention check.
func ensureGRHelpersReady(cs clientset.Interface, nodes []corev1.Node, peers []config.Peer) error {
	if len(nodes) == 0 || len(peers) == 0 {
		return errors.New("no nodes or GR helpers configured")
	}
	pods, err := k8s.FRRK8sPods(cs)
	if err != nil {
		return err
	}
	podsByNode := make(map[string]*corev1.Pod, len(pods))
	for _, pod := range pods {
		podsByNode[pod.Spec.NodeName] = pod
	}

	var pending error
	reconnect := map[string]frrcontainer.FRR{}
	for _, node := range nodes {
		pod, found := podsByNode[node.Name]
		if !found {
			pending = errors.Join(pending, fmt.Errorf("no FRR pod on node %s", node.Name))
			continue
		}
		neighbors, err := frr.NeighborsInfo(executor.ForPod(pod.Namespace, pod.Name, k8s.FRRContainerName))
		if err != nil {
			pending = errors.Join(pending, fmt.Errorf("query node %s neighbors: %w", node.Name, err))
			continue
		}
		for _, peer := range peers {
			neighbor, found := neighbors[peer.IP]
			if !found || !neighbor.Connected || neighbor.GRInfo.RemoteGrMode != "Helper" {
				pending = errors.Join(pending, fmt.Errorf("helper %s is not established on node %s", peer.IP, node.Name))
				continue
			}
			if neighbor.GRInfo.RBit {
				pending = errors.Join(pending, fmt.Errorf("helper %s advertises the restart bit on node %s", peer.IP, node.Name))
				reconnect[peer.FRR.Name] = peer.FRR
			}
		}
	}
	for _, helper := range reconnect {
		out, err := helper.Exec("vtysh", "-c", "clear bgp *")
		if err != nil {
			return fmt.Errorf("reconnect helper %s: %w: %s", helper.Name, err, out)
		}
	}
	return pending
}

// checkPrefixesOnNode checks that prefixes have at one kernel route on the
// the node. This function get routes directly from the node `docker exec ip
// route` and not from `vtysh show route` because we need to get routes while
// FRR process restarts.
func checkPrefixesOnNode(node string, prefixes []string) error {
	exc := executor.ForContainer(node)
	routes, err := container.BGPRoutes(exc, "")
	if err != nil {
		return err
	}

	for _, prefix := range prefixes {
		want, err := netip.ParsePrefix(prefix)
		if err != nil {
			return err
		}

		if _, exist := routes[want]; !exist {
			return fmt.Errorf("no route for prefix %s was found on node %s", prefix, node)
		}
	}
	return nil
}
