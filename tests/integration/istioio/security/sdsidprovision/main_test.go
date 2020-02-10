// Copyright 2020 Istio Authors
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

package sdsidprovision

import (
	"testing"

	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/framework/components/environment"
	"istio.io/istio/pkg/test/framework/components/istio"
)

var (
	ist istio.Instance
)

func TestMain(m *testing.M) {
	framework.
		NewSuite("provision_identity_through_SDS", m).
		SetupOnEnv(environment.Kube, istio.Setup(&ist, enableSDSIdProvisioning)).
		RequireEnvironment(environment.Kube).
		Run()
}

func enableSDSIdProvisioning(cfg *istio.Config) {
	if cfg == nil {
		return
	}
	cfg.Values["global.sds.enabled"] = "true"
	cfg.Values["global.sds.udsPath"] = "unix:/var/run/sds/uds_path"
	cfg.Values["global.sds.token.aud"] = "istio-ca"

	cfg.Values["global.mtls.enabled"] = "true"

	cfg.Values["nodeagent.enabled"] = "true"
	cfg.Values["nodeagent.image"] = "node-agent-k8s"
	cfg.Values["nodeagent.env.CA_PROVIDER"] = "Citadel"
	cfg.Values["nodeagent.env.CA_ADDR"] = "istio-citadel.istio-system:8060"
	cfg.Values["nodeagent.env.VALID_TOKEN"] = "true"

	cfg.Values["security.components.nodeAgent.enabled"] = "true"
}
