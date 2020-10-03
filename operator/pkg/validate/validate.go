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

package validate

import (
	"fmt"
	"reflect"

	"github.com/ghodss/yaml"

	"istio.io/api/operator/v1alpha1"
	operator_v1alpha1 "istio.io/istio/operator/pkg/apis/istio/v1alpha1"
	"istio.io/istio/operator/pkg/metrics"
	"istio.io/istio/operator/pkg/name"
	"istio.io/istio/operator/pkg/util"
	"istio.io/istio/pkg/config/mesh"
	"istio.io/istio/pkg/util/gogoprotomarshal"
)

var (
	// DefaultValidations maps a data path to a validation function.
	DefaultValidations = map[string]ValidatorFunc{
		"Values": func(path util.Path, i interface{}) util.Errors {
			return CheckValues(i)
		},
		"MeshConfig":                         validateMeshConfig,
		"Hub":                                validateHub,
		"Tag":                                validateTag,
		"AddonComponents":                    validateAddonComponents,
		"Components.IngressGateways[*].Name": validateGatewayName,
		"Components.EgressGateways[*].Name":  validateGatewayName,
	}
	// requiredValues lists all the values that must be non-empty.
	requiredValues = map[string]bool{}
)

// CheckIstioOperator validates the operator CR.
func CheckIstioOperator(iop *operator_v1alpha1.IstioOperator, checkRequiredFields bool) error {
	if iop == nil || iop.Spec == nil {
		return nil
	}

	errs := CheckIstioOperatorSpec(iop.Spec, checkRequiredFields)
	return errs.ToError()
}

// CheckIstioOperatorSpec validates the values in the given Installer spec, using the field map DefaultValidations to
// call the appropriate validation function. checkRequiredFields determines whether missing mandatory fields generate
// errors.
func CheckIstioOperatorSpec(is *v1alpha1.IstioOperatorSpec, checkRequiredFields bool) (errs util.Errors) {
	if is == nil {
		return util.Errors{}
	}

	return util.AppendErrs(errs, Validate(DefaultValidations, is, nil, checkRequiredFields))
}

// Validate function below is used by third party for integrations and has to be public

// Validate validates the values of the tree using the supplied Func.
func Validate(validations map[string]ValidatorFunc, structPtr interface{}, path util.Path, checkRequired bool) (errs util.Errors) {
	scope.Debugf("validate with path %s, %v (%T)", path, structPtr, structPtr)
	if structPtr == nil {
		return nil
	}
	if util.IsStruct(structPtr) {
		scope.Debugf("validate path %s, skipping struct type %T", path, structPtr)
		return nil
	}
	if !util.IsPtr(structPtr) {
		metrics.CRValidationFailure.Increment()
		return util.NewErrs(fmt.Errorf("validate path %s, value: %v, expected ptr, got %T", path, structPtr, structPtr))
	}
	structElems := reflect.ValueOf(structPtr).Elem()
	if !util.IsStruct(structElems) {
		metrics.CRValidationFailure.Increment()
		return util.NewErrs(fmt.Errorf("validate path %s, value: %v, expected struct, got %T", path, structElems, structElems))
	}

	if util.IsNilOrInvalidValue(structElems) {
		return
	}

	for i := 0; i < structElems.NumField(); i++ {
		fieldName := structElems.Type().Field(i).Name
		fieldValue := structElems.Field(i)
		kind := structElems.Type().Field(i).Type.Kind()
		if a, ok := structElems.Type().Field(i).Tag.Lookup("json"); ok && a == "-" {
			continue
		}

		scope.Debugf("Checking field %s", fieldName)
		switch kind {
		case reflect.Struct:
			errs = util.AppendErrs(errs, Validate(validations, fieldValue.Addr().Interface(), append(path, fieldName), checkRequired))
		case reflect.Map:
			newPath := append(path, fieldName)
			errs = util.AppendErrs(errs, validateLeaf(validations, newPath, fieldValue.Interface(), checkRequired))
			for _, key := range fieldValue.MapKeys() {
				nnp := append(newPath, key.String())
				errs = util.AppendErrs(errs, validateLeaf(validations, nnp, fieldValue.MapIndex(key), checkRequired))
			}
		case reflect.Slice:
			for i := 0; i < fieldValue.Len(); i++ {
				newValue := fieldValue.Index(i).Interface()
				newPath := append(path, indexPathForSlice(fieldName, i))
				if util.IsStruct(newValue) || util.IsPtr(newValue) {
					errs = util.AppendErrs(errs, Validate(validations, newValue, newPath, checkRequired))
				} else {
					errs = util.AppendErrs(errs, validateLeaf(validations, newPath, newValue, checkRequired))
				}
			}
		case reflect.Ptr:
			if util.IsNilOrInvalidValue(fieldValue.Elem()) {
				continue
			}
			newPath := append(path, fieldName)
			if fieldValue.Elem().Kind() == reflect.Struct {
				errs = util.AppendErrs(errs, Validate(validations, fieldValue.Interface(), newPath, checkRequired))
			} else {
				errs = util.AppendErrs(errs, validateLeaf(validations, newPath, fieldValue, checkRequired))
			}
		default:
			if structElems.Field(i).CanInterface() {
				errs = util.AppendErrs(errs, validateLeaf(validations, append(path, fieldName), fieldValue.Interface(), checkRequired))
			}
		}
	}
	if len(errs) > 0 {
		metrics.CRValidationFailure.Increment()
	}
	return errs
}

func validateLeaf(validations map[string]ValidatorFunc, path util.Path, val interface{}, checkRequired bool) util.Errors {
	pstr := path.String()
	msg := fmt.Sprintf("validate %s:%v(%T) ", pstr, val, val)
	if util.IsValueNil(val) || util.IsEmptyString(val) {
		if checkRequired && requiredValues[pstr] {
			return util.NewErrs(fmt.Errorf("field %s is required but not set", util.ToYAMLPathString(pstr)))
		}
		msg += fmt.Sprintf("validate %s: OK (empty value)", pstr)
		scope.Debug(msg)
		return nil
	}

	vf, ok := getValidationFuncForPath(validations, path)
	if !ok {
		msg += fmt.Sprintf("validate %s: OK (no validation)", pstr)
		scope.Debug(msg)
		// No validation defined.
		return nil
	}
	scope.Debug(msg)
	return vf(path, val)
}

func validateMeshConfig(path util.Path, root interface{}) util.Errors {
	vs, err := yaml.Marshal(root)
	if err != nil {
		return util.Errors{err}
	}
	defaultMesh := mesh.DefaultMeshConfig()
	// ApplyMeshConfigDefaults allows unknown fields, so we first check for unknown fields
	if err := gogoprotomarshal.ApplyYAMLStrict(string(vs), &defaultMesh); err != nil {
		return util.Errors{fmt.Errorf("failed to unmarshall mesh config: %v", err)}
	}
	// This method will also perform validation automatically
	if _, validErr := mesh.ApplyMeshConfigDefaults(string(vs)); validErr != nil {
		return util.Errors{validErr}
	}
	return nil
}

func validateHub(path util.Path, val interface{}) util.Errors {
	return validateWithRegex(path, val, ReferenceRegexp)
}

func validateTag(path util.Path, val interface{}) util.Errors {
	return validateWithRegex(path, val, TagRegexp)
}

func validateAddonComponents(path util.Path, val interface{}) util.Errors {
	valMap, ok := val.(map[string]*v1alpha1.ExternalComponentSpec)
	if !ok {
		return util.NewErrs(fmt.Errorf("validateAddonComponents(%s) bad type %T, want map[string]*ExternalComponentSpec", path, val))
	}

	for key := range valMap {
		cn := name.ComponentName(key)
		if name.BundledAddonComponentNamesMap[cn] && (cn == name.TitleCase(cn)) {
			return util.NewErrs(fmt.Errorf("invalid addon component name: %s, expect component name starting with lower-case character", key))
		}
	}

	return nil
}

func validateGatewayName(path util.Path, val interface{}) util.Errors {
	valStr, ok := val.(string)
	if !ok {
		return util.NewErrs(fmt.Errorf("validateGatewayName(%s) bad type %T, want string", path, val))
	}
	if valStr == "" {
		// will fall back to default gateway name: istio-ingressgateway and istio-egressgateway
		return nil
	}
	return validateWithRegex(path, val, ObjectNameRegexp)
}
