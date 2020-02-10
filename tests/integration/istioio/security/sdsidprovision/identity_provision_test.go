package sdsidprovision

import (
	"testing"

	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/istioio"
)

func TestIdentityProvisionThroughSDS(t *testing.T) {
	framework.
		NewTest(t).
		Run(istioio.NewBuilder("tasks__security__citadel_config__auth_sds").
			Add(istioio.Script{Input: istioio.Path("scripts/setup_and_verify_mutual_tls.txt")}).
			Add(istioio.MultiPodWait("foo")).
			Add(istioio.MultiPodWait("bar")).
			Add(istioio.Script{Input: istioio.Path("scripts/deploy_policies_and_rbac_roles.txt")}).
			Defer(istioio.Script{Input: istioio.Path("scripts/cleanup.txt")}).
			Build())
}
