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

package controller

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
	"github.com/labbs/tofu-tf-operator/internal/runner"
)

const (
	// DefaultRunnerServiceAccount is the ServiceAccount managed by the operator for the runner pods.
	DefaultRunnerServiceAccount = "tofu-runner"

	runnerContainer = "runner"
	// runnerUID is the user of the runner image.
	runnerUID = 65532
	// terminationGracePeriod lets the engine persist the state after an interrupt.
	terminationGracePeriod = 150
	// maxNameLength is the length limit of the names used as label values, such as Job names.
	maxNameLength = 63
)

// stepJobName returns the name of the Job running a step of a Run, short enough
// to be used as a label value.
func stepJobName(runName string, step runner.Step) string {
	name := runName + "-" + string(step)
	if len(name) <= maxNameLength {
		return name
	}
	sum := sha256.Sum256([]byte(runName))
	suffix := "-" + hex.EncodeToString(sum[:])[:8] + "-" + string(step)
	return runName[:maxNameLength-len(suffix)] + suffix
}

// runnerServiceAccount is the ServiceAccount used by the runner pods of a Workspace.
func runnerServiceAccount(ws *infrav1alpha1.Workspace) string {
	if ws.Spec.Runner.ServiceAccountName != "" {
		return ws.Spec.Runner.ServiceAccountName
	}
	return DefaultRunnerServiceAccount
}

// buildStepJob returns the Job executing a step of a Run.
func buildStepJob(run *infrav1alpha1.Run, ws *infrav1alpha1.Workspace, step runner.Step, image string) (*batchv1.Job, error) {
	labels := map[string]string{
		infrav1alpha1.WorkspaceLabel:  ws.Name,
		infrav1alpha1.RunLabel:        run.Name,
		"app.kubernetes.io/name":      "tofu-runner",
		"app.kubernetes.io/component": string(step),
	}
	podLabels := maps.Clone(ws.Spec.Runner.PodLabels)
	if podLabels == nil {
		podLabels = map[string]string{}
	}
	maps.Copy(podLabels, labels)

	env, err := runnerEnv(run, ws, step)
	if err != nil {
		return nil, err
	}
	envFrom := append([]corev1.EnvFromSource{}, ws.Spec.EnvFrom...)
	for _, from := range ws.Spec.VarsFrom {
		source := corev1.EnvFromSource{Prefix: runner.VarPrefix}
		ref := corev1.LocalObjectReference{Name: from.Name}
		if from.Kind == "Secret" {
			source.SecretRef = &corev1.SecretEnvSource{LocalObjectReference: ref, Optional: new(from.Optional)}
		} else {
			source.ConfigMapRef = &corev1.ConfigMapEnvSource{LocalObjectReference: ref, Optional: new(from.Optional)}
		}
		envFrom = append(envFrom, source)
	}

	volumes := []corev1.Volume{
		{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	mounts := []corev1.VolumeMount{
		{Name: "workspace", MountPath: runner.DefaultWorkDir},
		{Name: "tmp", MountPath: "/tmp"},
	}
	if git := ws.Spec.Source.Git; git != nil && git.SecretRef != nil {
		volumes = append(volumes, corev1.Volume{Name: "git-credentials", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: git.SecretRef.Name, DefaultMode: ptr.To[int32](0o440)},
		}})
		mounts = append(mounts, corev1.VolumeMount{Name: "git-credentials", MountPath: runner.DefaultGitCredentialsDir, ReadOnly: true})
	}
	if oci := ws.Spec.Source.OCI; oci != nil && oci.SecretRef != nil {
		volumes = append(volumes, corev1.Volume{Name: "oci-credentials", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: oci.SecretRef.Name, DefaultMode: ptr.To[int32](0o440)},
		}})
		mounts = append(mounts, corev1.VolumeMount{Name: "oci-credentials", MountPath: runner.DefaultOCICredentialsDir, ReadOnly: true})
	}
	if ref := planStorageSecret(ws); ref != "" {
		volumes = append(volumes, corev1.Volume{Name: "plan-storage", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: ref, DefaultMode: ptr.To[int32](0o440)},
		}})
		mounts = append(mounts, corev1.VolumeMount{Name: "plan-storage", MountPath: runner.DefaultPlanStorageDir, ReadOnly: true})
	}
	if cm := ws.Spec.Source.ConfigMap; cm != nil {
		volumes = append(volumes, corev1.Volume{Name: "source", VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: cm.Name}},
		}})
		mounts = append(mounts, corev1.VolumeMount{Name: "source", MountPath: runner.DefaultConfigMapDir, ReadOnly: true})
	}

	if ws.Spec.Runner.Image != "" {
		image = ws.Spec.Runner.Image
	}
	var deadline *int64
	if ws.Spec.Runner.Timeout != nil {
		deadline = new(int64(ws.Spec.Runner.Timeout.Seconds()))
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      stepJobName(run.Name, step),
			Namespace: run.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			// A failed apply is never retried automatically.
			BackoffLimit:          ptr.To[int32](0),
			ActiveDeadlineSeconds: deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels, Annotations: ws.Spec.Runner.PodAnnotations},
				Spec: corev1.PodSpec{
					RestartPolicy:                 corev1.RestartPolicyNever,
					ServiceAccountName:            runnerServiceAccount(ws),
					TerminationGracePeriodSeconds: ptr.To[int64](terminationGracePeriod),
					NodeSelector:                  ws.Spec.Runner.NodeSelector,
					Tolerations:                   ws.Spec.Runner.Tolerations,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   new(true),
						RunAsUser:      ptr.To[int64](runnerUID),
						RunAsGroup:     ptr.To[int64](runnerUID),
						FSGroup:        ptr.To[int64](runnerUID),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Volumes: volumes,
					Containers: []corev1.Container{{
						Name:                     runnerContainer,
						Image:                    image,
						Args:                     []string{string(step)},
						Env:                      env,
						EnvFrom:                  envFrom,
						Resources:                ws.Spec.Runner.Resources,
						VolumeMounts:             mounts,
						WorkingDir:               runner.DefaultWorkDir,
						TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: new(false),
							ReadOnlyRootFilesystem:   new(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
				},
			},
		},
	}
	return job, nil
}

// runnerEnv returns the environment of the runner container: the user's
// variables first, then the contract with the runner, then the input variables.
// Later entries take precedence, so the contract cannot be overridden by mistake.
func runnerEnv(run *infrav1alpha1.Run, ws *infrav1alpha1.Workspace, step runner.Step) ([]corev1.EnvVar, error) {
	env := append([]corev1.EnvVar{}, ws.Spec.Env...)
	add := func(name, value string) {
		if value != "" {
			env = append(env, corev1.EnvVar{Name: name, Value: value})
		}
	}
	add(runner.EnvNamespace, run.Namespace)
	add(runner.EnvRunName, run.Name)
	add(runner.EnvRunUID, string(run.UID))
	add(runner.EnvWorkspaceName, ws.Name)
	add(runner.EnvWorkspaceUID, string(ws.UID))
	add(runner.EnvOperation, string(run.Spec.Operation))
	add(runner.EnvEngine, string(ws.Spec.Engine.Type))
	add(runner.EnvEngineVersion, ws.Spec.Engine.Version)
	add(runner.EnvRevision, run.Spec.Revision)
	add(runner.EnvBackendType, string(ws.Spec.Backend.Type))
	add(runner.EnvBackendCustom, ws.Spec.Backend.Custom)
	if ws.Spec.Outputs != nil {
		add(runner.EnvOutputsSecret, ws.Spec.Outputs.SecretName)
	}
	if step == runner.StepApply && run.Status.Plan != nil {
		add(runner.EnvPlanDigest, run.Status.Plan.Digest)
	}

	storage := ws.Spec.PlanStorage
	add(runner.EnvPlanStorage, string(storage.Type))
	if s3 := storage.S3; s3 != nil {
		add(runner.EnvS3Bucket, s3.Bucket)
		add(runner.EnvS3Prefix, s3.Prefix)
		add(runner.EnvS3Region, s3.Region)
		add(runner.EnvS3Endpoint, s3.Endpoint)
		if s3.PathStyle {
			add(runner.EnvS3PathStyle, "true")
		}
	}
	if pg := storage.Postgres; pg != nil {
		add(runner.EnvPostgresTable, pg.Table)
	}

	src := ws.Spec.Source
	switch {
	case src.Git != nil:
		add(runner.EnvSourceKind, string(runner.SourceGit))
		add(runner.EnvGitURL, src.Git.URL)
		add(runner.EnvGitPath, src.Git.Path)
	case src.OCI != nil:
		add(runner.EnvSourceKind, string(runner.SourceOCI))
		add(runner.EnvOCIURL, src.OCI.URL)
		add(runner.EnvOCIPath, src.OCI.Path)
		if src.OCI.Insecure {
			add(runner.EnvOCIInsecure, "true")
		}
	case src.ConfigMap != nil:
		add(runner.EnvSourceKind, string(runner.SourceConfigMap))
	default:
		add(runner.EnvSourceKind, string(runner.SourceInline))
		add(runner.EnvInlineSource, src.Inline)
	}

	for _, v := range ws.Spec.Vars {
		name := runner.VarPrefix + v.Name
		switch {
		case v.ValueFrom != nil && v.ValueFrom.SecretKeyRef != nil:
			env = append(env, corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: v.ValueFrom.SecretKeyRef}})
		case v.ValueFrom != nil && v.ValueFrom.ConfigMapKeyRef != nil:
			env = append(env, corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: v.ValueFrom.ConfigMapKeyRef}})
		case v.Value != nil:
			value, err := variableValue(v.Value.Raw)
			if err != nil {
				return nil, fmt.Errorf("variable %s: %w", v.Name, err)
			}
			env = append(env, corev1.EnvVar{Name: name, Value: value})
		}
	}
	return env, nil
}

// planStorageSecret returns the Secret holding the credentials of the plan storage, if any.
func planStorageSecret(ws *infrav1alpha1.Workspace) string {
	storage := ws.Spec.PlanStorage
	switch {
	case storage.S3 != nil && storage.S3.SecretRef != nil:
		return storage.S3.SecretRef.Name
	case storage.Postgres != nil:
		return storage.Postgres.SecretRef.Name
	}
	return ""
}

// variableValue converts a JSON value to the -var syntax: strings are passed as
// is, other values as JSON, which the engine parses according to the declared type.
func variableValue(raw []byte) (string, error) {
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str, nil
	}
	// Compacting the raw JSON keeps large numbers exact.
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return "", err
	}
	return compact.String(), nil
}
