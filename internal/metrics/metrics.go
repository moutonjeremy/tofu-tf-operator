/*
Copyright 2026.

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

// Package metrics defines the Prometheus metrics of the operator, served by the
// metrics endpoint of the manager.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Label names.
const (
	labelNamespace = "namespace"
	labelWorkspace = "workspace"
)

var (
	// RunsTotal counts the finished Runs.
	RunsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tofu_operator_runs_total",
		Help: "Finished Runs, by Workspace, operation and final phase.",
	}, []string{labelNamespace, labelWorkspace, "operation", "phase"})

	// StepDuration measures the Jobs of the plan and apply steps.
	StepDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "tofu_operator_step_duration_seconds",
		Help:    "Duration of the plan and apply Jobs, by step and outcome.",
		Buckets: []float64{5, 15, 30, 60, 120, 300, 600, 1200, 1800, 3600},
	}, []string{"step", "result"})

	// DriftDetectedTotal counts the drift detections that found changes.
	DriftDetectedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tofu_operator_drift_detected_total",
		Help: "Drift detection plans that found changes, by Workspace.",
	}, []string{labelNamespace, labelWorkspace})

	// WorkspaceReady reports the Ready condition of the Workspaces.
	WorkspaceReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tofu_operator_workspace_ready",
		Help: "1 when the Workspace is Ready, 0 otherwise. The reason is the one of the Ready condition.",
	}, []string{labelNamespace, labelWorkspace, "reason"})

	// NotificationsTotal counts the notifications sent to providers.
	NotificationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tofu_operator_notifications_total",
		Help: "Notifications sent to NotificationProviders, by result (success or failure).",
	}, []string{labelNamespace, "provider", "result"})
)

func init() {
	ctrlmetrics.Registry.MustRegister(RunsTotal, StepDuration, DriftDetectedTotal, WorkspaceReady, NotificationsTotal)
}

// SetWorkspaceReady records the Ready condition of a Workspace, replacing the previous reason.
func SetWorkspaceReady(namespace, workspace, reason string, ready bool) {
	WorkspaceReady.DeletePartialMatch(prometheus.Labels{labelNamespace: namespace, labelWorkspace: workspace})
	value := 0.0
	if ready {
		value = 1
	}
	WorkspaceReady.WithLabelValues(namespace, workspace, reason).Set(value)
}

// ForgetWorkspace removes the series of a deleted Workspace.
func ForgetWorkspace(namespace, workspace string) {
	labels := prometheus.Labels{labelNamespace: namespace, labelWorkspace: workspace}
	WorkspaceReady.DeletePartialMatch(labels)
	RunsTotal.DeletePartialMatch(labels)
	DriftDetectedTotal.DeletePartialMatch(labels)
}
