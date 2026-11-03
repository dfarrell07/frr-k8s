// SPDX-License-Identifier:Apache-2.0

package tests

// Temporary CI validation instrumentation. Keep this separate from the GR
// fixture and its unchanged bidirectional route-retention assertions.

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/metallb/frrk8stests/pkg/dump"
	"github.com/metallb/frrk8stests/pkg/infra"
	"github.com/metallb/frrk8stests/pkg/k8s"
	"github.com/onsi/ginkgo/v2"
	"go.universe.tf/e2etest/pkg/executor"
	frrcontainer "go.universe.tf/e2etest/pkg/frr/container"
	corev1 "k8s.io/api/core/v1"
	clientset "k8s.io/client-go/kubernetes"
)

// Copied from the independently controlled scratch observer, with address-family
// selection and terminal poll coverage accounting for this CI validation.
//
//go:embed testdata/gr-route-watch.py
var grRouteWatch string

const grObserverDuration = 135 * time.Second
const grObserverPollInterval = 10 * time.Millisecond
const grObserverOwnerLabel = "frr-k8s.gr-ci-observer.owner"

type grObserver struct {
	node, name, path string
	prefixes         []string
	started          time.Time
	cmd              *exec.Cmd
	done             chan struct{}
	waitErr          error
	artifactErr      error
	removed          bool
}

type grCIObservers struct {
	dir, owner  string
	observers   []*grObserver
	cancel      context.CancelFunc
	armed       bool
	expectedAFI int
}

type grObserverRecord struct {
	Kind              string          `json:"kind"`
	Present           json.RawMessage `json:"present"`
	WindowComplete    *bool           `json:"window_complete"`
	AbsenceObserved   *bool           `json:"absence_observed"`
	ObserverLoss      *bool           `json:"observer_loss"`
	QueryErrors       *int            `json:"query_errors"`
	TerminationSignal json.RawMessage `json:"termination_signal"`
	RequestedDuration float64         `json:"requested_duration_seconds"`
	Elapsed           float64         `json:"elapsed_seconds"`
	Samples           int             `json:"samples"`
	MissedDeadlines   *int            `json:"missed_poll_deadlines"`
	IntervalMaxMS     *float64        `json:"interval_max_ms"`
}

func grCICommand(timeout time.Duration, command string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %v: %w (%v): %s", command, args, err, ctx.Err(), out)
	}
	return string(out), nil
}

func startGRCIObservers(cs clientset.Interface, nodes []corev1.Node, prefixes []string, helpers []*frrcontainer.FRR, dutPrefix string) (*grCIObservers, error) {
	if len(nodes) == 0 || len(prefixes) == 0 || len(helpers) == 0 {
		return nil, errors.New("GR observers need nodes, prefixes and an existing FRR helper image")
	}
	advertised, err := netip.ParsePrefix(dutPrefix)
	if err != nil {
		return nil, err
	}
	expectedAFI := 1
	if advertised.Addr().Is6() {
		expectedAFI = 2
	}
	testDir := filepath.Join(dump.ReportPath, strings.ReplaceAll(ginkgo.CurrentSpecReport().LeafNodeText, " ", "_"))
	if err := os.MkdirAll(testDir, 0755); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(testDir, "gr-ci-")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	group := &grCIObservers{dir: dir, owner: filepath.Base(dir), cancel: cancel, expectedAFI: expectedAFI}
	if err := os.WriteFile(filepath.Join(dir, "observer.py"), []byte(grRouteWatch), 0644); err != nil {
		return group, err
	}
	if err := dumpGRCIRuntime(cs, dir, "before", expectedAFI, time.Time{}); err != nil {
		return group, err
	}
	// Use the already-present helper image by immutable local ID: no image pull,
	// package installation, or dependency on Python inside the kind node image.
	image, err := grCICommand(10*time.Second, executor.ContainerRuntime, "inspect", "--format", "{{.Image}}", helpers[0].Name)
	if err != nil {
		return group, err
	}
	image = strings.TrimSpace(image)
	if image == "" {
		return group, errors.New("FRR helper image ID is empty")
	}
	type target struct {
		Name, Container string
		Prefixes        []string
	}
	var targets []target
	for _, node := range nodes {
		targets = append(targets, target{"node-" + node.Name, node.Name, prefixes})
	}
	// Helper kernel presence cannot prove subsecond BGP-RIB membership or all
	// three node nexthops. The original bidirectional 1Hz BGP assertion remains.
	for _, helper := range helpers {
		targets = append(targets, target{"helper-" + helper.Name, helper.Name, []string{dutPrefix}})
	}
	metadata, err := json.MarshalIndent(map[string]any{"image_id": image, "targets": targets,
		"table": 254, "protocol": nil, "poll_ms": grObserverPollInterval.Milliseconds(), "duration_seconds": grObserverDuration.Seconds(),
		"nodes": nodes, "expected_afi": expectedAFI}, "", "  ")
	if err != nil {
		return group, err
	}
	if err := os.WriteFile(filepath.Join(dir, "observers.json"), metadata, 0644); err != nil {
		return group, err
	}
	for i, target := range targets {
		observer := &grObserver{node: target.Name, name: fmt.Sprintf("frr-%s-%d", group.owner, i),
			prefixes: target.Prefixes, path: filepath.Join(dir, target.Name+"-routes.jsonl"), done: make(chan struct{})}
		output, err := os.Create(observer.path)
		if err != nil {
			return group, err
		}
		args := []string{"run", "--pull=never", "--interactive", "--name", observer.name,
			"--label", grObserverOwnerLabel + "=" + group.owner,
			"--network", "container:" + target.Container, "--entrypoint", "python3", image,
			"-u", "-", "--duration", fmt.Sprint(grObserverDuration.Seconds()),
			"--poll-ms", fmt.Sprint(grObserverPollInterval.Milliseconds()), "--table", "254"}
		args = append(args, target.Prefixes...)
		observer.cmd = exec.CommandContext(ctx, executor.ContainerRuntime, args...)
		observer.cmd.Stdin = strings.NewReader(grRouteWatch)
		observer.cmd.Stdout, observer.cmd.Stderr = output, output
		observer.cmd.WaitDelay = time.Second
		observer.started = time.Now()
		if err := observer.cmd.Start(); err != nil {
			_ = output.Close()
			return group, err
		}
		group.observers = append(group.observers, observer)
		go func() {
			observer.waitErr = observer.cmd.Wait()
			exitCode := -1
			if observer.cmd.ProcessState != nil {
				exitCode = observer.cmd.ProcessState.ExitCode()
			}
			status, statusErr := json.Marshal(map[string]any{"exit_code": exitCode, "error": fmt.Sprint(observer.waitErr)})
			observer.artifactErr = errors.Join(output.Close(), statusErr,
				os.WriteFile(observer.path+".exit.json", status, 0644))
			close(observer.done)
		}()
	}
	// Launch all observers together. This bound also ensures every 135s window
	// covers the original 120s Consistently check started immediately afterward.
	deadline := group.observers[0].started.Add(10 * time.Second)
	for time.Now().Before(deadline) {
		allReady := true
		for _, observer := range group.observers {
			rows, err := readGRCIRecords(observer.path, false)
			if err != nil {
				return group, err
			}
			ready := false
			for _, row := range rows {
				switch row.Kind {
				case "ready":
					var present map[string]bool
					if err := json.Unmarshal(row.Present, &present); err != nil {
						return group, fmt.Errorf("observer %s readiness: %w", observer.node, err)
					}
					ready = len(present) == len(observer.prefixes)
					for _, prefix := range observer.prefixes {
						ready = ready && present[prefix]
					}
				case "baseline_not_ready", "query_error", "observer_error", "fatal", "summary":
					return group, fmt.Errorf("observer %s failed before restart: %s", observer.node, row.Kind)
				}
			}
			select {
			case <-observer.done:
				return group, fmt.Errorf("observer %s exited before restart: %v", observer.node, observer.waitErr)
			default:
			}
			allReady = allReady && ready
		}
		if allReady {
			group.armed = true
			ginkgo.GinkgoWriter.Printf("GR CI observers ready: %s\n", dir)
			return group, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return group, errors.New("GR observers did not all become ready within 10 seconds")
}

func readGRCIRecords(path string, final bool) ([]grObserverRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		if final {
			return nil, fmt.Errorf("incomplete JSONL record in %s", path)
		}
		data = data[:bytes.LastIndexByte(data, '\n')+1]
	}
	var rows []grObserverRecord
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var row grObserverRecord
		if err := json.Unmarshal(line, &row); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (group *grCIObservers) finish() error {
	if group == nil {
		return nil
	}
	if !group.armed {
		return errors.Join(errors.New("GR observer setup did not complete"), group.cleanup())
	}
	var failures error
	for _, observer := range group.observers {
		select {
		case <-observer.done:
		default:
			timer := time.NewTimer(time.Until(observer.started.Add(grObserverDuration + 30*time.Second)))
			select {
			case <-observer.done:
				timer.Stop()
			case <-timer.C:
				failures = errors.Join(failures, fmt.Errorf("GR measurement fault: observer %s exceeded its bounded window", observer.node))
				continue
			}
		}
		rows, err := readGRCIRecords(observer.path, true)
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("GR measurement fault: %w", err))
			continue
		}
		if observer.artifactErr != nil {
			failures = errors.Join(failures, fmt.Errorf("GR measurement fault: observer %s artifacts: %w", observer.node, observer.artifactErr))
		}
		summaries := 0
		absenceObserved := false
		for _, row := range rows {
			switch row.Kind {
			case "baseline_not_ready", "query_error", "observer_error", "fatal", "incomplete":
				failures = errors.Join(failures, fmt.Errorf("GR measurement fault: observer %s reported %s; see %s", observer.node, row.Kind, observer.path))
			case "presence_transition":
				var present *bool
				if err := json.Unmarshal(row.Present, &present); err != nil || present == nil {
					failures = errors.Join(failures, fmt.Errorf("GR measurement fault: observer %s transition present=%s: %v", observer.node, row.Present, err))
				} else {
					absenceObserved = absenceObserved || !*present
				}
			}
			if row.Kind != "summary" {
				continue
			}
			summaries++
			exitMatches := observer.waitErr == nil
			if row.AbsenceObserved != nil && *row.AbsenceObserved {
				absenceObserved = true
				var exitErr *exec.ExitError
				exitMatches = errors.As(observer.waitErr, &exitErr) && exitErr.ExitCode() == 1
			}
			// One boundary slot is allowed. A sparse window or silent pause in
			// the final sleep cannot qualify as a successful 10ms measurement.
			minimumSamples := int(grObserverDuration/grObserverPollInterval) - 1
			if row.WindowComplete == nil || !*row.WindowComplete || row.ObserverLoss == nil || *row.ObserverLoss ||
				row.QueryErrors == nil || *row.QueryErrors != 0 || row.AbsenceObserved == nil ||
				string(row.TerminationSignal) != "null" || row.RequestedDuration != grObserverDuration.Seconds() ||
				row.Elapsed < grObserverDuration.Seconds() || row.Samples < minimumSamples ||
				row.MissedDeadlines == nil || *row.MissedDeadlines != 0 || row.IntervalMaxMS == nil || *row.IntervalMaxMS < 0 || !exitMatches {
				values, _ := json.Marshal(row)
				failures = errors.Join(failures, fmt.Errorf("GR measurement fault: observer %s: %s exit=%v; see %s", observer.node, values, observer.waitErr, observer.path))
			}
		}
		if absenceObserved {
			failures = errors.Join(failures, fmt.Errorf("GR kernel-route retention failure: observer %s saw a missing prefix; see %s", observer.node, observer.path))
		}
		if summaries != 1 {
			failures = errors.Join(failures, fmt.Errorf("GR measurement fault: observer %s has %d summaries; see %s", observer.node, summaries, observer.path))
		}
	}
	// Stop any timed-out observer before the following FRR diagnostic reads.
	if failures != nil {
		failures = errors.Join(failures, group.cleanup())
	}
	return failures
}

func (group *grCIObservers) earliestStart() time.Time {
	if group == nil || len(group.observers) == 0 {
		return time.Time{}
	}
	started := group.observers[0].started
	for _, observer := range group.observers[1:] {
		if observer.started.Before(started) {
			started = observer.started
		}
	}
	return started
}

func (group *grCIObservers) checkRestartCoverage(completed time.Time) error {
	started := group.earliestStart()
	if started.IsZero() {
		return errors.New("GR measurement fault: no observer deadlines for restart completion")
	}
	deadline := started.Add(grObserverDuration)
	// Both timestamps retain Go's monotonic clock component. Starting the bound
	// before the observer process launches is conservative about its true end.
	within := !completed.IsZero() && !completed.After(deadline)
	data, err := json.MarshalIndent(map[string]any{"restart_completed": completed,
		"earliest_conservative_observer_deadline": deadline, "within_window": within,
		"deadline_minus_completion_seconds": deadline.Sub(completed).Seconds(), "comparison_clock": "Go monotonic"}, "", "  ")
	if err != nil {
		return err
	}
	err = os.WriteFile(filepath.Join(group.dir, "restart-observer-coverage.json"), data, 0644)
	if !within {
		return errors.Join(err, fmt.Errorf("GR measurement fault: restart completion %s exceeded observer deadline %s", completed, deadline))
	}
	return err
}

func (group *grCIObservers) cleanup() error {
	if group == nil {
		return nil
	}
	group.cancel()
	var failures error
	for _, observer := range group.observers {
		if observer.removed {
			continue
		}
		out, err := grCICommand(10*time.Second, executor.ContainerRuntime, "inspect", observer.name)
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		var metadata []struct {
			ID     string `json:"Id"`
			Config struct{ Labels map[string]string }
		}
		if err := json.Unmarshal([]byte(out), &metadata); err != nil || len(metadata) != 1 ||
			metadata[0].Config.Labels[grObserverOwnerLabel] != group.owner || metadata[0].ID == "" {
			failures = errors.Join(failures, fmt.Errorf("cannot verify ownership of observer %s: %v", observer.name, err))
			continue
		}
		_, err = grCICommand(15*time.Second, executor.ContainerRuntime, "rm", "--force", metadata[0].ID)
		failures = errors.Join(failures, err)
		observer.removed = err == nil
		select {
		case <-observer.done:
		case <-time.After(2 * time.Second):
			failures = errors.Join(failures, fmt.Errorf("observer client %s did not exit after cleanup", observer.name))
		}
	}
	return failures
}

func dumpGRCIRuntime(cs clientset.Interface, dir, phase string, expectedAFI int, windowStart time.Time) error {
	pods, err := k8s.FRRK8sPods(cs)
	if err != nil {
		return err
	}
	metadata, err := json.MarshalIndent(pods, "", "  ")
	if err != nil {
		return err
	}
	var failures error
	failures = errors.Join(failures, os.WriteFile(filepath.Join(dir, phase+"-pods-image-ids.json"), metadata, 0644))
	for _, pod := range pods {
		utcVerified := false
		for _, status := range pod.Status.ContainerStatuses {
			if status.ImageID == "" {
				failures = errors.Join(failures, fmt.Errorf("pod %s container %s has no image ID", pod.Name, status.Name))
			}
		}
		for _, capture := range []struct {
			container, name string
			args            []string
		}{
			{k8s.FRRContainerName, "timezone", []string{"date", "+%Z %z"}},
			{k8s.FRRContainerName, "zebra-client", []string{"vtysh", "-d", "zebra", "-c", "show zebra client"}},
			{k8s.FRRContainerName, "frr-sha256", []string{"sha256sum", "/usr/lib/frr/bgpd", "/usr/lib/frr/zebra", "/usr/lib/frr/watchfrr", "/usr/bin/vtysh", "/usr/lib/libfrr.so.0"}},
			{"frr-metrics", "metrics-sha256", []string{"sha256sum", "/etc/frr_metrics/frr-metrics", "/usr/bin/vtysh", "/usr/lib/libfrr.so.0"}},
		} {
			args := []string{"exec", pod.Name, "-n", pod.Namespace, "-c", capture.container, "--"}
			readStarted := time.Now()
			out, err := grCICommand(10*time.Second, executor.Kubectl, append(args, capture.args...)...)
			readDuration := time.Since(readStarted)
			path := filepath.Join(dir, phase+"-"+pod.Name+"-"+capture.name+".log")
			failures = errors.Join(failures, err, os.WriteFile(path, []byte(out), 0644))
			if capture.name == "timezone" {
				utcVerified = err == nil && strings.TrimSpace(out) == "UTC +0000"
				if !utcVerified {
					failures = errors.Join(failures, fmt.Errorf("GR measurement fault: pod %s default timezone must be UTC +0000, got %q", pod.Name, out))
				}
			}
			if phase == "after" && capture.name == "zebra-client" && err == nil {
				syncErr := checkGRCIZebraSync(out, expectedAFI)
				rendered, completionErr := checkGRCIZebraCompletion(out, windowStart, readDuration)
				validationErr := errors.Join(syncErr, completionErr)
				if validationErr != nil {
					failures = errors.Join(failures, fmt.Errorf("pod %s GR completion: %w; see %s", pod.Name, validationErr, path))
				}
				var parsed, lower, upper any
				if !rendered.IsZero() {
					parsed, lower, upper = rendered, rendered.Add(-time.Second-readDuration), rendered.Add(2*time.Second)
				}
				bounds, marshalErr := json.MarshalIndent(map[string]any{"expected_afi": expectedAFI,
					"window_start": windowStart, "original_120s_deadline": windowStart.Add(2 * time.Minute),
					"rendered_sync_time": parsed, "completion_lower_bound": lower, "completion_upper_bound": upper,
					"read_duration_seconds": readDuration.Seconds(), "dut_utc_verified": utcVerified,
					"qualified": utcVerified && validationErr == nil, "validation_error": fmt.Sprint(validationErr),
					"comparison_clock":    "UTC wall time; FRR integer monotime conversion",
					"rounding_assumption": "stable realtime/monotonic offset; FRR inter-read pause bounded by full successful command duration"}, "", "  ")
				failures = errors.Join(failures, marshalErr,
					os.WriteFile(filepath.Join(dir, phase+"-"+pod.Name+"-gr-completion-bounds.json"), bounds, 0644))
			}
		}
	}
	for _, helper := range infra.FRRContainers {
		if helper.RouterConfig.VRF != "" {
			continue
		}
		out, err := grCICommand(10*time.Second, executor.ContainerRuntime, "inspect", helper.Name)
		failures = errors.Join(failures, err, os.WriteFile(filepath.Join(dir, phase+"-helper-"+helper.Name+"-image.json"), []byte(out), 0644))
		out, err = grCICommand(10*time.Second, executor.ContainerRuntime, "exec", helper.Name, "sha256sum",
			"/usr/lib/frr/bgpd", "/usr/lib/frr/zebra", "/usr/lib/frr/watchfrr", "/usr/bin/vtysh", "/usr/lib/libfrr.so.0")
		failures = errors.Join(failures, err, os.WriteFile(filepath.Join(dir, phase+"-helper-"+helper.Name+"-sha256.log"), []byte(out), 0644))
	}
	return failures
}

// Validate only the active routing client, excluding bgp [1] notifications and
// Stale Client Information. Keep the complete original output in the artifact.
func checkGRCIZebraSync(output string, expectedAFI int) error {
	body, err := grCIBGPDefault(output)
	if err != nil {
		return err
	}
	if !strings.Contains(body, "Capabilities : Graceful Restart") {
		return errors.New("default bgp client does not have Graceful Restart enabled")
	}
	expectedFound := false
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "AFI ") {
			continue
		}
		var afi int
		var state string
		if n, err := fmt.Sscanf(line, "AFI %d enabled, route sync %s", &afi, &state); err != nil || n != 2 || afi < 1 || state != "DONE" {
			return fmt.Errorf("default bgp route synchronization is incomplete: %s", line)
		}
		expectedFound = expectedFound || afi == expectedAFI
	}
	if !expectedFound {
		return fmt.Errorf("default bgp client is missing enabled AFI %d synchronization state", expectedAFI)
	}
	return nil
}

func grCIBGPDefault(output string) (string, error) {
	client, clients := "", 0
	for _, block := range strings.Split(output, "Client:") {
		name, body, _ := strings.Cut(block, "\n")
		if strings.TrimSpace(name) == "bgp" {
			client, clients = body, clients+1
		}
	}
	if clients != 1 {
		return "", fmt.Errorf("expected one active bgp client, got %d", clients)
	}
	client, _, _ = strings.Cut(client, "Stale Client Information")
	for _, block := range strings.Split(client, "VRF :") {
		name, body, _ := strings.Cut(block, "\n")
		if strings.TrimSpace(name) != "default" {
			continue
		}
		return body, nil
	}
	return "", errors.New("active bgp client has no default VRF")
}

func checkGRCIZebraCompletion(output string, windowStart time.Time, readDuration time.Duration) (time.Time, error) {
	body, err := grCIBGPDefault(output)
	if err != nil {
		return time.Time{}, err
	}
	const prefix = "Route sync finished at "
	var timestamps []string
	for _, line := range strings.Split(body, "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, prefix) {
			timestamps = append(timestamps, strings.TrimPrefix(line, prefix))
		}
	}
	if len(timestamps) != 1 {
		return time.Time{}, fmt.Errorf("expected one active bgp/default completion timestamp, got %d", len(timestamps))
	}
	// ANSIC has no timezone or fractional seconds. The caller validates the
	// container's default UTC timezone with OS date, outside the observed window.
	rendered, err := time.ParseInLocation(time.ANSIC, timestamps[0], time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse bgp/default completion timestamp: %w", err)
	}
	if windowStart.IsZero() {
		return rendered, errors.New("GR measurement fault: missing original observation start")
	}
	if readDuration < 0 {
		return rendered, errors.New("GR measurement fault: negative zebra-client command duration")
	}
	// FRR stores floor(event MONOTIME), then renders time(NULL) minus
	// (floor(current MONOTIME) - stored). Under a stable realtime/monotonic
	// offset, the whole successful command's monotonic duration bounds a pause
	// between those two reads. Use [rendered-1s-readDuration, rendered+2s],
	// a rounding envelope rather than an exact subsecond completion timestamp.
	// Require that whole envelope inside the ORIGINAL 120s check.
	lower, upper := rendered.Add(-time.Second-readDuration), rendered.Add(2*time.Second)
	deadline := windowStart.Add(2 * time.Minute)
	if lower.Before(windowStart) || upper.After(deadline) {
		return rendered, fmt.Errorf("GR lifecycle completion rounding envelope [%s, %s] outside original interval [%s, %s]",
			lower, upper, windowStart, deadline)
	}
	return rendered, nil
}
