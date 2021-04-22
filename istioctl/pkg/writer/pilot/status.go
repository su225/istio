// Copyright Istio Authors
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

package pilot

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	xdsapi "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	xdsstatus "github.com/envoyproxy/go-control-plane/envoy/service/status/v3"

	"istio.io/istio/istioctl/pkg/multixds"
	"istio.io/istio/pilot/pkg/xds"
	v3 "istio.io/istio/pilot/pkg/xds/v3"
	"istio.io/pkg/log"
)

// StatusWriter enables printing of sync status using multiple []byte Istiod responses
type StatusWriter struct {
	Writer  io.Writer
	XDSCols []string
}

type writerStatus struct {
	pilot string
	xds.SyncStatus
}

// XdsStatusWriter enables printing of sync status using multiple xdsapi.DiscoveryResponse Istiod responses
type XdsStatusWriter struct {
	Writer io.Writer
}

type xdsWriterStatus struct {
	proxyID        string
	istiodID       string
	istiodVersion  string
	clusterStatus  string
	listenerStatus string
	routeStatus    string
	endpointStatus string
}

// PrintAll takes a slice of Pilot syncz responses and outputs them using a tabwriter
func (s *StatusWriter) PrintAll(statuses map[string][]byte) error {
	w, fullStatus, err := s.setupStatusPrint(statuses)
	if err != nil {
		return err
	}
	for _, status := range fullStatus {
		if err := s.statusPrintln(w, status); err != nil {
			return err
		}
	}
	return w.Flush()
}

// PrintSingle takes a slice of Pilot syncz responses and outputs them using a tabwriter filtering for a specific pod
func (s *StatusWriter) PrintSingle(statuses map[string][]byte, proxyName string) error {
	w, fullStatus, err := s.setupStatusPrint(statuses)
	if err != nil {
		return err
	}
	for _, status := range fullStatus {
		if strings.Contains(status.ProxyID, proxyName) {
			if err := s.statusPrintln(w, status); err != nil {
				return err
			}
		}
	}
	return w.Flush()
}

func (s *StatusWriter) setupStatusPrint(statuses map[string][]byte) (*tabwriter.Writer, []*writerStatus, error) {
	w := new(tabwriter.Writer).Init(s.Writer, 0, 8, 5, ' ', 0)
	_, _ = fmt.Fprintf(w, "NAME\t%s\tISTIOD\tVERSION\n", strings.Join(s.XDSCols, "\t"))
	fullStatus := make([]*writerStatus, 0, len(statuses))
	for pilot, status := range statuses {
		var ss []*writerStatus
		err := json.Unmarshal(status, &ss)
		if err != nil {
			return nil, nil, err
		}
		for _, s := range ss {
			s.pilot = pilot
		}
		fullStatus = append(fullStatus, ss...)
	}
	sort.Slice(fullStatus, func(i, j int) bool {
		return fullStatus[i].ProxyID < fullStatus[j].ProxyID
	})
	return w, fullStatus, nil
}

func (s *StatusWriter) statusPrintln(w io.Writer, sw *writerStatus) error {
	statuses := make([]string, 0)
	for _, ty := range s.XDSCols {
		curStatus := xdsStatus(sw.Statuses[v3.GetShortType(ty)].NonceSent,
			sw.Statuses[v3.GetShortType(ty)].NonceAcked)

		// This is required to support the case where istioctl is newer,
		// but control plane (pilot/istiod) is older
		switch ty {
		case "CDS":
			curStatus = xdsStatus(sw.ClusterSent, sw.ClusterAcked) //nolint:staticcheck
		case "LDS":
			curStatus = xdsStatus(sw.ListenerSent, sw.ListenerAcked) //nolint:staticcheck
		case "RDS":
			curStatus = xdsStatus(sw.RouteSent, sw.RouteAcked) //nolint:staticcheck
		case "EDS":
			curStatus = xdsStatus(sw.EndpointSent, sw.EndpointAcked) //nolint:staticcheck
		}

		statuses = append(statuses, curStatus)
	}

	version := sw.IstioVersion
	if version == "" {
		// If we can't find an Istio version (talking to a 1.1 pilot), fallback to the proxy version
		// This is misleading, as the proxy version isn't always the same as the Istio version,
		// but it is better than not providing any information.
		version = sw.ProxyVersion + "*"
	}
	_, _ = fmt.Fprintf(w, "%v\t%v\t%v\t%v\n",
		sw.ProxyID, strings.Join(statuses, "\t"),
		sw.pilot, version)
	return nil
}

func xdsStatus(sent, acked string) string {
	if sent == "" {
		return "NOT SENT"
	}
	if sent == acked {
		return "SYNCED"
	}
	// acked will be empty string when there is never Acknowledged
	if acked == "" {
		return "STALE (Never Acknowledged)"
	}
	// Since the Nonce changes to uuid, so there is no more any time diff info
	return "STALE"
}

// PrintAll takes a slice of Istiod syncz responses and outputs them using a tabwriter
func (s *XdsStatusWriter) PrintAll(statuses map[string]*xdsapi.DiscoveryResponse) error {
	w, fullStatus, err := s.setupStatusPrint(statuses)
	if err != nil {
		return err
	}
	for _, status := range fullStatus {
		if err := xdsStatusPrintln(w, status); err != nil {
			return err
		}
	}
	if w != nil {
		return w.Flush()
	}
	return nil
}

func (s *XdsStatusWriter) setupStatusPrint(drs map[string]*xdsapi.DiscoveryResponse) (*tabwriter.Writer, []*xdsWriterStatus, error) {
	// Gather the statuses before printing so they may be sorted
	var fullStatus []*xdsWriterStatus
	var w *tabwriter.Writer
	for _, dr := range drs {
		for _, resource := range dr.Resources {
			switch resource.TypeUrl {
			case "type.googleapis.com/envoy.service.status.v3.ClientConfig":
				clientConfig := xdsstatus.ClientConfig{}
				err := resource.UnmarshalTo(&clientConfig)
				if err != nil {
					return nil, nil, fmt.Errorf("could not unmarshal ClientConfig: %w", err)
				}
				cds, lds, eds, rds := getSyncStatus(clientConfig.GetXdsConfig())
				cp := multixds.CpInfo(dr)
				fullStatus = append(fullStatus, &xdsWriterStatus{
					proxyID:        clientConfig.GetNode().GetId(),
					istiodID:       cp.ID,
					istiodVersion:  cp.Info.Version,
					clusterStatus:  cds,
					listenerStatus: lds,
					routeStatus:    rds,
					endpointStatus: eds,
				})
				if len(fullStatus) == 0 {
					return nil, nil, fmt.Errorf("no proxies found (checked %d istiods)", len(drs))
				}

				w = new(tabwriter.Writer).Init(s.Writer, 0, 8, 5, ' ', 0)
				_, _ = fmt.Fprintln(w, "NAME\tCDS\tLDS\tEDS\tRDS\tISTIOD\tVERSION")

				sort.Slice(fullStatus, func(i, j int) bool {
					return fullStatus[i].proxyID < fullStatus[j].proxyID
				})
			default:
				for _, resource := range dr.Resources {
					s.Writer.Write(resource.Value) // nolint: errcheck
				}
				fullStatus = nil
			}
		}
	}

	return w, fullStatus, nil
}

func xdsStatusPrintln(w io.Writer, status *xdsWriterStatus) error {
	_, err := fmt.Fprintf(w, "%v\t%v\t%v\t%v\t%v\t%v\t%v\n",
		status.proxyID,
		status.clusterStatus, status.listenerStatus, status.endpointStatus, status.routeStatus,
		status.istiodID, status.istiodVersion)
	return err
}

func getSyncStatus(configs []*xdsstatus.PerXdsConfig) (cds, lds, eds, rds string) {
	for _, config := range configs {
		switch val := config.PerXdsConfig.(type) {
		case *xdsstatus.PerXdsConfig_ListenerConfig:
			lds = config.Status.String()
		case *xdsstatus.PerXdsConfig_ClusterConfig:
			cds = config.Status.String()
		case *xdsstatus.PerXdsConfig_RouteConfig:
			rds = config.Status.String()
		case *xdsstatus.PerXdsConfig_EndpointConfig:
			eds = config.Status.String()
		case *xdsstatus.PerXdsConfig_ScopedRouteConfig:
			// ignore; Istiod doesn't send these
		default:
			log.Infof("PerXdsConfig unexpected type %T\n", val)
		}
	}
	return
}
