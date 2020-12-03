// Copyright Istio Authors.
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
// limitations under the License.

package verifier

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	v1batch "k8s.io/api/batch/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	apimachinery_schema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes/scheme"

	"istio.io/istio/istioctl/pkg/clioptions"
	operator_istio "istio.io/istio/operator/pkg/apis/istio"
	"istio.io/istio/operator/pkg/apis/istio/v1alpha1"
	"istio.io/istio/operator/pkg/controlplane"
	"istio.io/istio/operator/pkg/translate"
	"istio.io/istio/operator/pkg/util"
	"istio.io/istio/operator/pkg/util/clog"
)

var (
	istioOperatorGVR = apimachinery_schema.GroupVersionResource{
		Group:    v1alpha1.SchemeGroupVersion.Group,
		Version:  v1alpha1.SchemeGroupVersion.Version,
		Resource: "istiooperators",
	}
)

// StatusVerifier checks status of certain resources like deployment,
// jobs and also verifies count of certain resource types.
type StatusVerifier struct {
	istioNamespace   string
	manifestsPath    string
	kubeconfig       string
	context          string
	filenames        []string
	controlPlaneOpts clioptions.ControlPlaneOptions
	logger           clog.Logger
	iop              *v1alpha1.IstioOperator
}

// NewStatusVerifier creates a new instance of post-install verifier
// which checks the status of various resources from the manifest.
// TODO(su225): This is doing too many things. Refactor: break it down
func NewStatusVerifier(istioNamespace, manifestsPath, kubeconfig, context string,
	filenames []string, controlPlaneOpts clioptions.ControlPlaneOptions,
	logger clog.Logger, installedIOP *v1alpha1.IstioOperator) *StatusVerifier {
	if logger == nil {
		logger = clog.NewDefaultLogger()
	}
	return &StatusVerifier{
		istioNamespace:   istioNamespace,
		manifestsPath:    manifestsPath,
		filenames:        filenames,
		controlPlaneOpts: controlPlaneOpts,
		logger:           logger,
		kubeconfig:       kubeconfig,
		context:          context,
		iop:              installedIOP,
	}
}

// Verify implements Verifier interface. Here we check status of deployment
// and jobs, count various resources for verification.
func (v *StatusVerifier) Verify() error {
	if v.iop != nil {
		return v.verifyFinalIOP()
	}
	if len(v.filenames) == 0 {
		return v.verifyInstallIOPRevision()
	}
	return v.verifyInstall()
}

func (v *StatusVerifier) verifyInstallIOPRevision() error {
	iop, err := v.operatorFromCluster(v.controlPlaneOpts.Revision)
	if err != nil {
		return fmt.Errorf("could not load IstioOperator from cluster: %v.  Use --filename", err)
	}
	if v.manifestsPath != "" {
		iop.Spec.InstallPackagePath = v.manifestsPath
	}
	crdCount, istioDeploymentCount, err := v.verifyPostInstallIstioOperator(
		iop, fmt.Sprintf("in cluster operator %s", iop.GetName()))
	return v.reportStatus(crdCount, istioDeploymentCount, err)
}

func (v *StatusVerifier) verifyFinalIOP() error {
	crdCount, istioDeploymentCount, err := v.verifyPostInstallIstioOperator(
		v.iop, fmt.Sprintf("IOP:%s", v.iop.GetName()))
	return v.reportStatus(crdCount, istioDeploymentCount, err)
}

func (v *StatusVerifier) verifyInstall() error {
	// This is not a pre-check.  Check that the supplied resources exist in the cluster
	r := resource.NewBuilder(v.k8sConfig()).
		Unstructured().
		FilenameParam(false, &resource.FilenameOptions{Filenames: v.filenames}).
		Flatten().
		Do()
	if r.Err() != nil {
		return r.Err()
	}
	visitor := genericclioptions.ResourceFinderForResult(r).Do()
	crdCount, istioDeploymentCount, err := v.verifyPostInstall(
		visitor, strings.Join(v.filenames, ","))
	return v.reportStatus(crdCount, istioDeploymentCount, err)
}

func (v *StatusVerifier) verifyPostInstallIstioOperator(iop *v1alpha1.IstioOperator, filename string) (int, int, error) {
	t := translate.NewTranslator()

	cp, err := controlplane.NewIstioControlPlane(iop.Spec, t)
	if err != nil {
		return 0, 0, err
	}
	if err := cp.Run(); err != nil {
		return 0, 0, err
	}

	manifests, errs := cp.RenderManifest()
	if errs != nil && len(errs) > 0 {
		return 0, 0, errs.ToError()
	}

	builder := resource.NewBuilder(v.k8sConfig()).ContinueOnError().Unstructured()
	for cat, manifest := range manifests {
		for i, manitem := range manifest {
			reader := strings.NewReader(manitem)
			pseudoFilename := fmt.Sprintf("%s:%d generated from %s", cat, i, filename)
			builder = builder.Stream(reader, pseudoFilename)
		}
	}
	r := builder.Flatten().Do()
	if r.Err() != nil {
		return 0, 0, r.Err()
	}
	visitor := genericclioptions.ResourceFinderForResult(r).Do()
	// Indirectly RECURSE back into verifyPostInstall with the manifest we just generated
	generatedCrds, generatedDeployments, err := v.verifyPostInstall(
		visitor,
		fmt.Sprintf("generated from %s", filename))
	if err != nil {
		return generatedCrds, generatedDeployments, err
	}

	return generatedCrds, generatedDeployments, nil
}

func (v *StatusVerifier) verifyPostInstall(visitor resource.Visitor, filename string) (int, int, error) {
	crdCount := 0
	istioDeploymentCount := 0
	err := visitor.Visit(func(info *resource.Info, err error) error {
		if err != nil {
			return err
		}
		content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(info.Object)
		if err != nil {
			return err
		}
		un := &unstructured.Unstructured{Object: content}
		kind := un.GetKind()
		name := un.GetName()
		namespace := un.GetNamespace()
		kinds := findResourceInSpec(kind)
		if kinds == "" {
			kinds = strings.ToLower(kind) + "s"
		}
		if namespace == "" {
			namespace = "default"
		}
		switch kind {
		case "Deployment":
			deployment := &appsv1.Deployment{}
			err = info.Client.
				Get().
				Resource(kinds).
				Namespace(namespace).
				Name(name).
				VersionedParams(&meta_v1.GetOptions{}, scheme.ParameterCodec).
				Do(context.TODO()).
				Into(deployment)
			if err != nil {
				v.reportFailure(kind, name, namespace, err)
				return err
			}
			if err = verifyDeploymentStatus(deployment); err != nil {
				ivf := istioVerificationFailureError(filename, err)
				v.reportFailure(kind, name, namespace, ivf)
				return ivf
			}
			if namespace == v.istioNamespace && strings.HasPrefix(name, "istio") {
				istioDeploymentCount++
			}
		case "Job":
			job := &v1batch.Job{}
			err = info.Client.
				Get().
				Resource(kinds).
				Namespace(namespace).
				Name(name).
				VersionedParams(&meta_v1.GetOptions{}, scheme.ParameterCodec).
				Do(context.TODO()).
				Into(job)
			if err != nil {
				v.reportFailure(kind, name, namespace, err)
				return err
			}
			if err := verifyJobPostInstall(job); err != nil {
				ivf := istioVerificationFailureError(filename, err)
				v.reportFailure(kind, name, namespace, ivf)
				return ivf
			}
		case "IstioOperator":
			// It is not a problem if the cluster does not include the IstioOperator
			// we are checking.  Instead, verify the cluster has the things the
			// IstioOperator specifies it should have.

			// IstioOperator isn't part of pkg/config/schema/collections,
			// usual conversion not available.  Convert unstructured to string
			// and ask operator code to unmarshal.
			fixTimestampRelatedUnmarshalIssues(un)
			by := util.ToYAML(un)
			iop, err := operator_istio.UnmarshalIstioOperator(by, true)
			if err != nil {
				v.reportFailure(kind, name, namespace, err)
				return err
			}
			if v.manifestsPath != "" {
				iop.Spec.InstallPackagePath = v.manifestsPath
			}
			generatedCrds, generatedDeployments, err := v.verifyPostInstallIstioOperator(iop, filename)
			if err != nil {
				return err
			}
			crdCount += generatedCrds
			istioDeploymentCount += generatedDeployments
		default:
			result := info.Client.
				Get().
				Resource(kinds).
				Name(name).
				Do(context.TODO())
			if result.Error() != nil {
				result = info.Client.
					Get().
					Resource(kinds).
					Namespace(namespace).
					Name(name).
					Do(context.TODO())
				if result.Error() != nil {
					v.reportFailure(kind, name, namespace, result.Error())
					return istioVerificationFailureError(filename,
						fmt.Errorf("the required %s:%s is not ready due to: %v",
							kind, name, result.Error()))
				}
			}
			if kind == "CustomResourceDefinition" {
				crdCount++
			}
		}
		v.logger.LogAndPrintf("✔ %s: %s.%s checked successfully", kind, name, namespace)
		return nil
	})
	return crdCount, istioDeploymentCount, err
}

// Find an IstioOperator matching revision in the cluster.  The IstioOperators
// don't have a label for their revision, so we parse them and check .Spec.Revision
func (v *StatusVerifier) operatorFromCluster(revision string) (*v1alpha1.IstioOperator, error) {
	restConfig, err := v.k8sConfig().ToRESTConfig()
	if err != nil {
		return nil, err
	}
	client, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}
	iops, err := AllOperatorsInCluster(client)
	if err != nil {
		return nil, err
	}
	for _, iop := range iops {
		if iop.Spec.Revision == revision {
			return iop, nil
		}
	}
	return nil, fmt.Errorf("control plane revision %q not found", revision)
}

func (v *StatusVerifier) reportStatus(crdCount, istioDeploymentCount int, err error) error {
	v.logger.LogAndPrintf("Checked %v custom resource definitions", crdCount)
	v.logger.LogAndPrintf("Checked %v Istio Deployments", istioDeploymentCount)
	if istioDeploymentCount == 0 {
		v.logger.LogAndPrintf("! No Istio installation found")
		return fmt.Errorf("no Istio installation found")
	}
	if err != nil {
		// Don't return full error; it is usually an unwielded aggregate
		return fmt.Errorf("Istio installation failed") // nolint
	}
	v.logger.LogAndPrintf("✔ Istio is installed and verified successfully")
	return nil
}

func fixTimestampRelatedUnmarshalIssues(un *unstructured.Unstructured) {
	un.SetCreationTimestamp(meta_v1.Time{}) // UnmarshalIstioOperator chokes on these

	// UnmarshalIstioOperator fails because managedFields could contain time
	// and gogo/protobuf/jsonpb(v1.3.1) tries to unmarshal it as struct (the type
	// meta_v1.Time is really a struct) and fails.
	un.SetManagedFields([]meta_v1.ManagedFieldsEntry{})
}

// Find all IstioOperator in the cluster.
func AllOperatorsInCluster(client dynamic.Interface) ([]*v1alpha1.IstioOperator, error) {
	ul, err := client.
		Resource(istioOperatorGVR).
		List(context.TODO(), meta_v1.ListOptions{})
	if err != nil {
		return nil, err
	}
	retval := make([]*v1alpha1.IstioOperator, 0)
	for _, un := range ul.Items {
		fixTimestampRelatedUnmarshalIssues(&un)
		by := util.ToYAML(un.Object)
		iop, err := operator_istio.UnmarshalIstioOperator(by, true)
		if err != nil {
			return nil, err
		}
		retval = append(retval, iop)
	}
	return retval, nil
}

func istioVerificationFailureError(filename string, reason error) error {
	return fmt.Errorf("Istio installation failed, incomplete or does not match \"%s\": %v", filename, reason) // nolint
}

func (v *StatusVerifier) k8sConfig() *genericclioptions.ConfigFlags {
	return &genericclioptions.ConfigFlags{KubeConfig: &v.kubeconfig, Context: &v.context}
}

func (v *StatusVerifier) reportFailure(kind, name, namespace string, err error) {
	v.logger.LogAndPrintf("✘ %s: %s.%s: %v", kind, name, namespace, err)
}
