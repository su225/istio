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

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/hashicorp/go-multierror"
	"io"
	"istio.io/istio/pkg/kube"
	admit_v1 "k8s.io/api/admissionregistration/v1"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"istio.io/api/label"
	"istio.io/api/operator/v1alpha1"
	"istio.io/istio/operator/cmd/mesh"
	operator_istio "istio.io/istio/operator/pkg/apis/istio"
	iopv1alpha1 "istio.io/istio/operator/pkg/apis/istio/v1alpha1"
	"istio.io/istio/operator/pkg/manifest"
	"istio.io/istio/operator/pkg/util"
	"istio.io/istio/operator/pkg/util/clog"
	"istio.io/istio/pkg/config"
	v1 "k8s.io/api/core/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apimachinery_schema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/apimachinery/pkg/util/validation"
)

type revisionArgs struct {
	manifestsPath string
	name          string
	verbose       bool
	sections      []string
	output        string
}

const (
	IstioOperatorCRSection  = "ISTIO-OPERATOR-CR"
	ControlPlaneSection     = "CONTROL-PLANE"
	GatewaysSection         = "GATEWAYS"
	WebhooksSection         = "MUTATING-WEBHOOKS"
	NamespaceSummarySection = "NAMESPACE-SUMMARY"
	PodsSection             = "PODS"

	// TODO: This should be moved to istio/api:label package
	istioTag = "istio.io/tag"

	jsonFormat   = "json"
	tableFormat  = "table"
)

var (
	validFormats = []string{tableFormat, jsonFormat}

	defaultSections = []string{
		IstioOperatorCRSection,
		WebhooksSection,
		ControlPlaneSection,
		GatewaysSection,
	}

	verboseSections = []string{
		IstioOperatorCRSection,
		WebhooksSection,
		ControlPlaneSection,
		GatewaysSection,
		NamespaceSummarySection,
		PodsSection,
	}
)

var (
	istioOperatorGVR = apimachinery_schema.GroupVersionResource{
		Group:    iopv1alpha1.SchemeGroupVersion.Group,
		Version:  iopv1alpha1.SchemeGroupVersion.Version,
		Resource: "istiooperators",
	}

	revArgs = revisionArgs{}
)

func revisionCommand() *cobra.Command {
	revisionCmd := &cobra.Command{
		Use:     "revision",
		Short:   "Revision centric view of Istio deployment",
		Aliases: []string{"rev"},
	}
	revisionCmd.PersistentFlags().StringVarP(&revArgs.manifestsPath, "manifests", "d", "", mesh.ManifestsFlagHelpStr)
	revisionCmd.PersistentFlags().BoolVarP(&revArgs.verbose, "verbose", "v", false, "print customizations")
	revisionCmd.PersistentFlags().StringVarP(&revArgs.output, "output", "o", "table", "Output format for revision description")

	revisionCmd.AddCommand(revisionListCommand())
	revisionCmd.AddCommand(revisionDescribeCommand())
	return revisionCmd
}

// TODO(su225): Fix example, description - command documentation part
func revisionDescribeCommand() *cobra.Command {
	describeCmd := &cobra.Command{
		Use:     "describe",
		Example: `    istioctl experimental revision describe <revision>`,
		Short:   "Show details of a revision - customizations, number of pods pointing to it, istiod, gateways etc",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("revision is not specified")
			}
			if len(args) > 1 {
				return fmt.Errorf("exactly 1 revision should be specified")
			}
			isValidFormat := false
			for _, f := range validFormats {
				if revArgs.output == f {
					isValidFormat = true
					break
				}
			}
			if !isValidFormat {
				return fmt.Errorf("unknown format %s. It should be %#v", revArgs.output, validFormats)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			revArgs.name = args[0]
			if revArgs.verbose {
				revArgs.sections = verboseSections
			} else {
				revArgs.sections = defaultSections
			}
			if revArgs.name != "<default>" {
				if errs := validation.IsDNS1123Label(revArgs.name); len(errs) > 0 {
					return fmt.Errorf(strings.Join(errs, "\n"))
				}
			} else {
				revArgs.name = "" // TODO(su225): This case is broken. Fix it.
			}
			logger := clog.NewConsoleLogger(cmd.OutOrStdout(), cmd.ErrOrStderr(), scope)
			return printRevisionDescription(cmd.OutOrStdout(), &revArgs, logger)
		},
	}
	describeCmd.Flags().BoolVarP(&revArgs.verbose, "verbose", "v", false, "Dump all information related to the revision")
	return describeCmd
}

func revisionListCommand() *cobra.Command {
	listCmd := &cobra.Command{
		Use:     "list",
		Short:   "Show list of control plane and gateway revisions that are currently installed in cluster",
		Example: `   istioctl experimental revision list`,
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := clog.NewConsoleLogger(cmd.OutOrStdout(), cmd.ErrOrStderr(), scope)
			return revisionList(cmd.OutOrStdout(), &revArgs, logger)
		},
	}
	return listCmd
}

type podFilteredInfo struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Address   string `json:"address"`
	Status    v1.PodPhase `json:"status"`
	Age       string `json:"age"`
}

type istioOperatorCRInfo struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Profile   string `json:"profile"`
	Components []string `json:"components"`
	Customizations []iopDiff `json:"customizations"`
}

type mutatingWebhookConfigInfo struct {
	Name string 	`json:"name"`
	Revision string `json:"revision"`
	Tag string		`json:"tag"`
}

type revisionDescription struct {
	IstioOperatorCRs   []istioOperatorCRInfo        `json:"istio_operator_crs,omitempty"`
	Webhooks           []mutatingWebhookConfigInfo  `json:"webhooks,omitempty"`
	ControlPlanePods   []podFilteredInfo            `json:"control_plane_pods,omitempty"`
	IngressGatewayPods []podFilteredInfo            `json:"ingess_gateways,omitempty"`
	EgressGatewayPods  []podFilteredInfo            `json:"egress_gateways,omitempty"`
	NamespaceSummary   map[string]uint              `json:"namespace_summary,omitempty"`
	Pods               []podFilteredInfo 			`json:"pods,omitempty"`
}

func revisionList(writer io.Writer, args *revisionArgs, logger clog.Logger) error {
	client, err := newKubeClient(kubeconfig, configContext)
	if err != nil {
		return fmt.Errorf("cannot create kubeclient for kubeconfig=%s, context=%s: %v",
			kubeconfig, configContext, err)
	}

	revisions := map[string]*revisionDescription{}

	// Get a list of control planes which are installed in remote clusters
	// In this case, it is possible that they only have webhooks installed.
	webhooks, err := getWebhooks(context.Background(), client)
	if err != nil {
		return fmt.Errorf("error while listing mutating webhooks: %v", err)
	}
	for _, hook := range webhooks {
		rev := renderWithDefault(hook.GetLabels()[label.IstioRev], "default")
		tag := hook.GetLabels()[istioTag]
		ri, revPresent := revisions[rev]
		if revPresent {
			if tag != "" {
				ri.Webhooks = append(ri.Webhooks, mutatingWebhookConfigInfo{
					Name: hook.Name,
					Revision: rev,
					Tag: tag,
				})
			}
		} else {
			revisions[rev] = &revisionDescription{
				IstioOperatorCRs: []istioOperatorCRInfo{},
				Webhooks: []mutatingWebhookConfigInfo{{Name: hook.Name, Revision: rev, Tag: tag}},
			}
		}
	}

	// Get a list of all CRs which are installed in this cluster
	iopcrs, err := getAllIstioOperatorCRs(client)
	if err != nil {
		return fmt.Errorf("error while listing IstioOperator CRs: %v", err)
	}
	for _, iop := range iopcrs {
		if iop == nil {
			continue
		}
		rev := renderWithDefault(iop.Spec.GetRevision(), "default")
		ri, revPresent := revisions[rev]
		if revPresent {
			iopInfo := istioOperatorCRInfo{
				Namespace:      iop.GetNamespace(),
				Name:           iop.GetName(),
				Profile:        iop.Spec.GetProfile(),
				Components:     getEnabledComponents(iop.Spec),
				Customizations: nil,
			}
			if args.verbose {
				iopInfo.Customizations, err = getDiffs(iop, revArgs.manifestsPath, iop.Spec.GetProfile(), logger)
				if err != nil {
					return fmt.Errorf("error while finding customizations: %v", err)
				}
			}
			ri.IstioOperatorCRs = append(ri.IstioOperatorCRs, iopInfo)
		}
	}

	switch revArgs.output {
	case "json":
		return printJSON(writer, revisions)
	default:
		return printRevisionInfoTable(writer, args.verbose, revisions)
	}
}

func printRevisionInfoTable(writer io.Writer, verbose bool, revisions map[string]*revisionDescription) error {
	tw := new(tabwriter.Writer).Init(writer, 0, 8, 1, ' ', 0)
	if verbose {
		tw.Write([]byte("REVISION\tTAG\tISTIO-OPERATOR-CR\tPROFILE\tREQD-COMPONENTS\tCUSTOMIZATIONS\n"))
	} else {
		tw.Write([]byte("REVISION\tTAG\tISTIO-OPERATOR-CR\tPROFILE\tREQD-COMPONENTS\n"))
	}
	for r, ri := range revisions {
		rowId, tags := 0, []string{}
		for _, wh := range ri.Webhooks {
			tags = append(tags, wh.Tag)
		}
		if len(tags) == 0 {
			tags = append(tags, "<no-tag>")
		}
		for _, iop := range ri.IstioOperatorCRs {
			profile := effectiveProfile(iop.Profile)
			components := iop.Components
			qualifiedName := fmt.Sprintf("%s/%s", iop.Namespace, iop.Name)

			customizations := []string{}
			for _, c := range iop.Customizations {
				customizations = append(customizations, fmt.Sprintf("%s=%s", c.Path, c.Value))
			}
			if len(customizations) == 0 {
				customizations = append(customizations, "<no-customization>")
			}
			maxIopRows := max(max(1, len(components)), len(customizations))
			for i := 0; i < maxIopRows; i++ {
				var rowTag, rowRev string
				var rowIopName, rowProfile, rowComp, rowCust string
				if i == 0 {
					rowIopName = qualifiedName
					rowProfile = profile
				}
				if i < len(components) {
					rowComp = components[i]
				}
				if i < len(customizations) {
					rowCust = customizations[i]
				}
				if rowId < len(tags) {
					rowTag = tags[rowId]
				}
				if rowId == 0 {
					rowRev = r
				}
				if verbose {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
						rowRev, rowTag, rowIopName, rowProfile, rowComp, rowCust)
				} else {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
						rowRev, rowTag, rowIopName, rowProfile, rowComp)
				}
				rowId++
			}
		}
		for rowId < len(tags) {
			var rowRev, rowTag, rowIopName string
			if rowId == 0 {
				rowRev = r
			}
			if rowId == 0 {
				rowIopName = "<no-iop>"
			}
			rowTag = tags[rowId]
			if verbose {
				fmt.Fprintf(tw, "%s\t%s\t%s\t \t \t \n", rowRev, rowTag, rowIopName)
			} else {
				fmt.Fprintf(tw, "%s\t%s\t%s\t \t \n", rowRev, rowTag, rowIopName)
			}
			rowId++
		}
	}
	return tw.Flush()
}

func getAllIstioOperatorCRs(client kube.ExtendedClient) ([]*iopv1alpha1.IstioOperator, error) {
	ucrs, err := client.Dynamic().Resource(istioOperatorGVR).
		List(context.Background(), meta_v1.ListOptions{})
	if err != nil {
		return []*iopv1alpha1.IstioOperator{}, fmt.Errorf("cannot retrieve IstioOperator CRs: %v", err)
	}
	iopCRs := []*iopv1alpha1.IstioOperator{}
	for _, u := range ucrs.Items {
		u.SetCreationTimestamp(meta_v1.Time{})
		u.SetManagedFields([]meta_v1.ManagedFieldsEntry{})
		iop, err := operator_istio.UnmarshalIstioOperator(util.ToYAML(u.Object), true)
		if err != nil {
			return []*iopv1alpha1.IstioOperator{},
				fmt.Errorf("error while converting to IstioOperator CR - %s/%s: %v",
					u.GetNamespace(), u.GetName(), err)
		}
		iopCRs = append(iopCRs, iop)
	}
	return iopCRs, nil
}

func printRevisionDescription(w io.Writer, args *revisionArgs, logger clog.Logger) error {
	revision := args.name
	client, err := newKubeClientWithRevision(kubeconfig, configContext, revision)
	if err != nil {
		return fmt.Errorf("cannot create kubeclient for kubeconfig=%s, context=%s: %v",
			kubeconfig, configContext, err)
	}
	allIops, err := getAllIstioOperatorCRs(client)
	if err != nil {
		return fmt.Errorf("error while fetching IstioOperator CR: %v", err)
	}
	iopsInCluster := getIOPWithRevision(allIops, revision)
	allWebhooks, err := getWebhooks(context.Background(), client)
	if err != nil {
		return fmt.Errorf("error while fetching mutating webhook configurations: %v", err)
	}
	webhooks := getWebhooksWithRevision(allWebhooks, revision)
	controlPlanePods, err := getControlPlanePods(client)
	if err != nil {
		return fmt.Errorf("error while fetching control plane pods: %v", err)
	}
	// No webhook, no IOP and no control plane pod for a given revision
	// It means that the revision no longer exists in the cluster.
	if len(webhooks) == 0 && len(iopsInCluster) == 0 && len(controlPlanePods) == 0 {
		return fmt.Errorf("revision %s does not exist", revision)
	}

	revDescription := revisionDescription{}
	errs := &multierror.Error{}
	var pods []v1.Pod
	for _, s := range args.sections {
		switch s {
		case IstioOperatorCRSection:
			crsExtracted := []istioOperatorCRInfo{}
			for _, iop := range iopsInCluster {
				iopCustomizations, err := getDiffs(iop, args.manifestsPath, iop.Spec.GetProfile(), logger)
				if err != nil {
					errs = multierror.Append(err, errs.Errors...)
					continue
				}
				crInfo := istioOperatorCRInfo{
					Namespace: iop.Namespace,
					Name: iop.Name,
					Components: getEnabledComponents(iop.Spec),
					Customizations: iopCustomizations,
				}
				crsExtracted = append(crsExtracted, crInfo)
			}
			revDescription.IstioOperatorCRs = crsExtracted
		case WebhooksSection:
			whExtracted := []mutatingWebhookConfigInfo{}
			for _, wh := range webhooks {
				wh := mutatingWebhookConfigInfo{
					Name:     wh.Name,
					Revision: wh.GetLabels()[label.IstioRev],
					Tag:      wh.GetLabels()[istioTag],
				}
				whExtracted = append(whExtracted, wh)
			}
			revDescription.Webhooks = whExtracted
		case ControlPlaneSection:
			revDescription.ControlPlanePods = transformToFilteredPodInfo(controlPlanePods)
		case GatewaysSection:
			ingressPods, err := getIngressGateways(client)
			if err != nil {
				errs = multierror.Append(err, errs.Errors...)
			} else {
				revDescription.IngressGatewayPods = transformToFilteredPodInfo(ingressPods)
			}
			egressPods, err := getEgressGateways(client)
			if err != nil {
				errs = multierror.Append(err, errs.Errors...)
			} else {
				revDescription.EgressGatewayPods = transformToFilteredPodInfo(egressPods)
			}
		case NamespaceSummarySection:
			if pods == nil {
				pods, err = getPodsWithRevision(client)
			}
			if err != nil {
				errs = multierror.Append(err, errs.Errors...)
			} else {
				podPerNsCount := map[string]uint{}
				for _, pod := range pods {
					podPerNsCount[pod.Namespace]++
				}
				revDescription.NamespaceSummary = podPerNsCount
			}
		case PodsSection:
			if pods == nil {
				pods, err = getPodsWithRevision(client)
			}
			filteredPodInfo := []podFilteredInfo{}
			for _, p := range pods {
				podInfo := getFilteredPodInfo(&p)
				filteredPodInfo = append(filteredPodInfo, podInfo)
			}
			revDescription.Pods = filteredPodInfo
		default:
			return fmt.Errorf("unknown section: %s", s)
		}
	}
	switch revArgs.output {
	case jsonFormat:
		return printJSON(w, revDescription)
	case tableFormat:
		return printTable(w, args.sections, &revDescription)
	default:
		return fmt.Errorf("unknown format %s", revArgs.output)
	}
}

func printJSON(w io.Writer, res interface{}) error {
	out, err := json.MarshalIndent(res, "", "\t")
	if err != nil {
		return fmt.Errorf("error while marshaling to JSON: %v", err)
	}
	_, err = w.Write(out)
	fmt.Fprintln(w, string(out))
	return nil
}

func getWebhooksWithRevision(webhooks []admit_v1.MutatingWebhookConfiguration, revision string) []admit_v1.MutatingWebhookConfiguration {
	whFiltered := []admit_v1.MutatingWebhookConfiguration{}
	for _, wh := range webhooks {
		if wh.GetLabels()[label.IstioRev] == revision {
			whFiltered = append(whFiltered, wh)
		}
	}
	return whFiltered
}

func transformToFilteredPodInfo(pods []v1.Pod) []podFilteredInfo {
	pfilInfo := []podFilteredInfo{}
	for _, p := range pods {
		pfilInfo = append(pfilInfo, getFilteredPodInfo(&p))
	}
	return pfilInfo
}

func getFilteredPodInfo(pod *v1.Pod) podFilteredInfo {
	return podFilteredInfo{
		Namespace: pod.Namespace,
		Name: pod.Name,
		Address: pod.Status.PodIP,
		Status: pod.Status.Phase,
		Age: translateTimestampSince(pod.CreationTimestamp),
	}
}

func printTable(w io.Writer, sections []string, desc *revisionDescription) error {
	errs := &multierror.Error{}
	tablePrintFuncs := map[string]func(io.Writer, *revisionDescription)error{
		IstioOperatorCRSection: printIstioOperatorCRInfo,
		WebhooksSection: printWebhookInfo,
		ControlPlaneSection: printControlPlane,
		GatewaysSection: printGateways,
		NamespaceSummarySection: printNamespaceSummary,
		PodsSection: printPodsSection,
	}
	for _, s := range sections {
		f := tablePrintFuncs[s]
		if f == nil {
			errs = multierror.Append(fmt.Errorf("unknown section: %s", s), errs.Errors...)
			continue
		}
		err := f(w, desc)
		if err != nil {
			errs = multierror.Append(fmt.Errorf("error in section %s: %v", s, err))
		}
	}
	return errs.ErrorOrNil()
}

func printNamespaceSummary(w io.Writer, desc *revisionDescription) error {
	fmt.Println("\nNAMESPACE-SUMMARY")
	nsSummaryWriter := new(tabwriter.Writer).Init(w, 0, 0, 1, ' ', 0)
	fmt.Fprintln(nsSummaryWriter, "NAMESPACE\tPOD-COUNT")
	for ns, podCount := range desc.NamespaceSummary {
		fmt.Fprintf(nsSummaryWriter, "%s\t%d\n", ns, podCount)
	}
	var err error
	if err = nsSummaryWriter.Flush(); err != nil {
		return err
	}
	return err
}

func printIstioOperatorCRInfo(w io.Writer, desc *revisionDescription) error {
	fmt.Fprintf(w, "\nISTIO-OPERATOR-CR: (%d)", len(desc.IstioOperatorCRs))
	if len(desc.IstioOperatorCRs) == 0 {
		if len(desc.Webhooks) > 0 {
			fmt.Fprintln(w, "There are webhooks and Istiod could be external to the cluster")
		} else {
			// Ideally, it should not come here
			fmt.Fprintln(w, "No CRs found.")
		}
		return nil
	}
	for i, iop := range desc.IstioOperatorCRs {
		fmt.Fprintf(w, "\n%d. %s/%s\n", i+1, iop.Namespace, iop.Name)
		fmt.Fprintf(w, "  COMPONENTS:\n")
		for _, c := range iop.Components {
			fmt.Fprintf(w, "  - %s\n", c)
		}

		// For each IOP, print all customizations for it
		fmt.Fprintf(w, "  CUSTOMIZATIONS:\n")
		for _, customization := range iop.Customizations {
			fmt.Fprintf(w, "  - %s=%s\n", customization.Path, customization.Value)
		}
	}
	return nil
}

func printWebhookInfo(w io.Writer, desc *revisionDescription) error {
	fmt.Fprintf(w, "\nMUTATING-WEBHOOKS: (%d)\n", len(desc.Webhooks))
	if len(desc.Webhooks) == 0 {
		fmt.Fprintln(w, "No mutating webhook found for this revision. Something could be wrong with installation")
		return nil
	}
	tw := new(tabwriter.Writer).Init(w, 0, 0, 1, ' ', 0)
	tw.Write([]byte("WEBHOOK\tTAG\n"))
	for _, wh := range desc.Webhooks {
		tw.Write([]byte(fmt.Sprintf("%s\t%s\n", wh.Name, renderWithDefault(wh.Tag, "no-tag"))))
	}
	return tw.Flush()
}

func printControlPlane(w io.Writer, desc *revisionDescription) error {
	fmt.Fprintf(w, "\nCONTROL-PLANE-PODS (ISTIOD): (%d)\n", len(desc.ControlPlanePods))
	if len(desc.ControlPlanePods) == 0 {
		if len(desc.Webhooks) > 0 {
			fmt.Fprintln(w, "No Istiod found in this cluster for the revision. " +
				"However there are webhooks. It is possible that Istiod is external to this cluster or " +
				"perhaps it is not uninstalled properly")
		} else {
			fmt.Fprintln(w, "No Istiod or the webhook found in this cluster for the revision. Something could be wrong")
		}
		return nil
	}
	return printPodTable(w, desc.ControlPlanePods)
}

func printGateways(w io.Writer, desc *revisionDescription) error {
	if err := printIngressGateways(w, desc); err != nil {
		return fmt.Errorf("error while printing ingress-gateway info: %v", err)
	}
	if err := printEgressGateways(w, desc); err != nil {
		return fmt.Errorf("error while printing egress-gateway info: %v", err)
	}
	return nil
}

func printIngressGateways(w io.Writer, desc *revisionDescription) error {
	fmt.Fprintf(w, "\nINGRESS-GATEWAYS: (%d)\n", len(desc.IngressGatewayPods))
	if len(desc.IngressGatewayPods) == 0 {
		if isIngressGatewayEnabled(desc) {
			fmt.Fprintln(w, "Ingress gateway is enabled for this revision. However there are no such pods. " +
				"It could be that it is replaced by ingress-gateway from another revision (as it is still upgraded in-place) " +
				"or it could be some issue with installation")
		} else {
			fmt.Fprintln(w, "Ingress gateway is disabled for this revision")
		}
		return nil
	}
	if !isIngressGatewayEnabled(desc) {
		fmt.Fprintln(w, "WARNING: Ingress gateway is not enabled for this revision.")
	}
	return printPodTable(w, desc.IngressGatewayPods)
}

func printEgressGateways(w io.Writer, desc *revisionDescription) error {
	fmt.Fprintf(w, "\nEGRESS-GATEWAYS: (%d)\n", len(desc.IngressGatewayPods))
	if len(desc.EgressGatewayPods) == 0 {
		if isEgressGatewayEnabled(desc) {
			fmt.Fprintln(w, "Egress gateway is enabled for this revision. However there are no such pods. " +
				"It could be that it is replaced by egress-gateway from another revision (as it is still upgraded in-place) " +
				"or it could be some issue with installation")
		} else {
			fmt.Fprintln(w, "Egress gateway is disabled for this revision")
		}
		return nil
	}
	if !isEgressGatewayEnabled(desc) {
		fmt.Fprintln(w, "WARNING: Egress gateway is not enabled for this revision.")
	}
	return printPodTable(w, desc.EgressGatewayPods)
}

type istioGatewayType = string
const (
	ingress istioGatewayType = "ingress"
	egress istioGatewayType = "egress"
)

func isIngressGatewayEnabled(desc *revisionDescription) bool {
	return isGatewayTypeEnabled(desc, ingress)
}

func isEgressGatewayEnabled(desc *revisionDescription) bool {
	return isGatewayTypeEnabled(desc, egress)
}

func printPodsSection(w io.Writer, desc *revisionDescription) error {
	fmt.Fprintf(w, "\nPODS: (%d)\n", len(desc.Pods))
	if len(desc.Pods) == 0 {
		fmt.Fprintln(w, "No pod is pointing to this revision. So it is safe to delete")
		return nil
	}
	return printPodTable(w, desc.Pods)
}

func isGatewayTypeEnabled(desc *revisionDescription, gwType istioGatewayType) bool {
	for _, iopdesc := range desc.IstioOperatorCRs {
		for _, comp := range iopdesc.Components {
			if strings.HasPrefix(comp, gwType) {
				return true
			}
		}
	}
	return false
}

func getIOPWithRevision(iops []*iopv1alpha1.IstioOperator, revision string) []*iopv1alpha1.IstioOperator {
	filteredIOPs := []*iopv1alpha1.IstioOperator{}
	for _, iop := range iops {
		if iop.Spec == nil {
			continue
		}
		if iop.Spec.Revision == revision || (revision == "default" && len(iop.Spec.Revision) == 0) {
			filteredIOPs = append(filteredIOPs, iop)
		}
	}
	return filteredIOPs
}

func printPodTable(w io.Writer, pods []podFilteredInfo) error {
	podTableW := new(tabwriter.Writer).Init(w, 0, 0, 1, ' ', 0)
	fmt.Fprintln(podTableW, "NAMESPACE\tNAME\tADDRESS\tSTATUS\tAGE")
	for _, pod := range pods {
		fmt.Fprintf(podTableW, "%s\t%s\t%s\t%s\t%s\n",
			pod.Namespace, pod.Name, pod.Address, pod.Status, pod.Age)
	}
	return podTableW.Flush()
}

func getEnabledComponents(iops *v1alpha1.IstioOperatorSpec) []string {
	enabledComponents := []string{}
	if iops.Components.Base.Enabled.GetValue() {
		enabledComponents = append(enabledComponents, "base")
	}
	if iops.Components.Cni.Enabled.GetValue() {
		enabledComponents = append(enabledComponents, "cni")
	}
	if iops.Components.Pilot.Enabled.GetValue() {
		enabledComponents = append(enabledComponents, "istiod")
	}
	if iops.Components.IstiodRemote.Enabled.GetValue() {
		enabledComponents = append(enabledComponents, "istiod-remote")
	}
	for _, gw := range iops.Components.IngressGateways {
		if gw.Enabled.Value {
			enabledComponents = append(enabledComponents, fmt.Sprintf("ingress:%s", gw.GetName()))
		}
	}
	for _, gw := range iops.Components.EgressGateways {
		if gw.Enabled.Value {
			enabledComponents = append(enabledComponents, fmt.Sprintf("egress:%s", gw.GetName()))
		}
	}
	return enabledComponents
}

func getControlPlanePods(client kube.ExtendedClient) ([]v1.Pod, error) {
	return getPodsForComponent(client, "Pilot")
}

func getIngressGateways(client kube.ExtendedClient) ([]v1.Pod, error) {
	return getPodsForComponent(client, "IngressGateways")
}

func getEgressGateways(client kube.ExtendedClient) ([]v1.Pod, error) {
	return getPodsForComponent(client, "EgressGateways")
}

func getPodsForComponent(client kube.ExtendedClient, component string) ([]v1.Pod, error) {
	return getPodsWithSelector(client, istioNamespace, &meta_v1.LabelSelector{
		MatchLabels: map[string]string{
			label.IstioRev:               client.Revision(),
			label.IstioOperatorComponent: component,
		},
	})
}

func getPodsWithRevision(client kube.ExtendedClient) ([]v1.Pod, error) {
	return getPodsWithSelector(client, "", &meta_v1.LabelSelector{
		MatchLabels: map[string]string{
			label.IstioRev: client.Revision(),
		},
	})
}

func getPodsWithSelector(client kube.ExtendedClient, ns string, selector *meta_v1.LabelSelector) ([]v1.Pod, error) {
	labelSelector, err := meta_v1.LabelSelectorAsSelector(selector)
	if err != nil {
		return []v1.Pod{}, err
	}
	podList, err := client.CoreV1().Pods(ns).List(context.TODO(),
		meta_v1.ListOptions{LabelSelector: labelSelector.String()})
	if err != nil {
		return []v1.Pod{}, err
	}
	return podList.Items, nil
}

type iopDiff struct {
	Path string `json:"path"`
	Value string `json:"value"`
}

func getDiffs(installed *iopv1alpha1.IstioOperator, manifestsPath, profile string, l clog.Logger) ([]iopDiff, error) {
	setFlags := []string{"profile=" + profile}
	if manifestsPath != "" {
		setFlags = append(setFlags, fmt.Sprintf("installPackagePath=%s", manifestsPath))
	}
	_, base, err := manifest.GenerateConfig([]string{}, setFlags, true, nil, l)
	if err != nil {
		return []iopDiff{}, err
	}
	mapInstalled, err := config.ToMap(installed.Spec)
	if err != nil {
		return []iopDiff{}, err
	}
	mapBase, err := config.ToMap(base.Spec)
	if err != nil {
		return []iopDiff{}, err
	}
	return diffWalk("", "", mapInstalled, mapBase)
}

func diffWalk(path, separator string, obj interface{}, orig interface{}) ([]iopDiff, error) {
	switch v := obj.(type) {
	case map[string]interface{}:
		accum := make([]iopDiff, 0)
		typedOrig, ok := orig.(map[string]interface{})
		if ok {
			for key, vv := range v {
				childwalk, err := diffWalk(fmt.Sprintf("%s%s%s", path, separator, pathComponent(key)), ".", vv, typedOrig[key])
				if err != nil {
					return accum, err
				}
				accum = append(accum, childwalk...)
			}
		}
		return accum, nil
	case []interface{}:
		accum := make([]iopDiff, 0)
		typedOrig, ok := orig.([]interface{})
		if ok {
			for idx, vv := range v {
				indexwalk, err := diffWalk(fmt.Sprintf("%s[%d]", path, idx), ".", vv, typedOrig[idx])
				if err != nil {
					return accum, err
				}
				accum = append(accum, indexwalk...)
			}
		}
		return accum, nil
	case string:
		if v != orig && orig != nil {
			return []iopDiff{{Path: path, Value: fmt.Sprintf("%q", v)}}, nil
		}
	default:
		if v != orig && orig != nil {
			return []iopDiff{{Path: path, Value: fmt.Sprintf("%v", v)}}, nil
		}
	}
	return []iopDiff{}, nil
}

func renderWithDefault(s, def string) string {
	if s != "" {
		return s
	}
	return fmt.Sprintf("<%s>", def)
}

func effectiveProfile(profile string) string {
	if profile != "" {
		return profile
	}
	return "default"
}

func pathComponent(component string) string {
	if !strings.Contains(component, util.PathSeparator) {
		return component
	}
	return strings.ReplaceAll(component, util.PathSeparator, util.EscapedPathSeparator)
}

// Human-readable age.  (This is from kubectl pkg/describe/describe.go)
func translateTimestampSince(timestamp meta_v1.Time) string {
	if timestamp.IsZero() {
		return "<unknown>"
	}
	return duration.HumanDuration(time.Since(timestamp.Time))
}

func max(x, y int) int {
	if x > y {
		return x
	}
	return y
}
