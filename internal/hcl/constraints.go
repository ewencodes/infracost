package hcl

import (
	"encoding/json"

	"github.com/hashicorp/go-version"
	"github.com/zclconf/go-cty/cty/gocty"
)

var (
	// AWSVersionConstraintVolumeTags is the version constraint for the Terraform aws
	// provider that supports propagating default tags to volume tags. See
	// https://github.com/hashicorp/terraform-provider-aws/issues/19890 for more
	// information.
	AWSVersionConstraintVolumeTags = version.Must(version.NewVersion("5.39.0"))
)

// ProviderConstraints represents the constraints for a providers within a
// module. This can be used to check if Infracost functionality is applicable to
// a given run.
type ProviderConstraints struct {
	AWS version.Constraints
}

// UnmarshalJSON parses the JSON representation of the ProviderConstraints and
// sets the constraints for the sub providers.
func (p *ProviderConstraints) UnmarshalJSON(b []byte) error {
	var out map[string]string
	err := json.Unmarshal(b, &out)
	if err != nil {
		return err
	}

	if v, ok := out["aws"]; ok {
		constraints, err := version.NewConstraint(v)
		if err == nil {
			p.AWS = constraints
		}
	}

	return nil
}

// MarshalJSON returns the JSON representation of the ProviderConstraints.
// This is used to serialize the constraints for the sub providers.
func (p *ProviderConstraints) MarshalJSON() ([]byte, error) {
	out := map[string]string{}
	if p == nil {
		return json.Marshal(&out)
	}

	if p.AWS != nil {
		out["aws"] = p.AWS.String()
	} else {
		out["aws"] = ""
	}

	return json.Marshal(out)
}

// NewProviderConstraints parses the provider blocks for any Terraform
// configuration blocks if found it will attempt to return a ProviderConstraints
// struct from the required_providers configuration. Currently, we only support
// AWS provider constraints.
func NewProviderConstraints(blocks Blocks) *ProviderConstraints {
	terraformBlocks := blocks.OfType("terraform")
	for _, block := range terraformBlocks {
		req := block.GetChildBlock("required_providers")
		if req == nil {
			continue
		}

		def := req.Values().AsValueMap()
		for provider, body := range def {
			if provider != "aws" {
				continue
			}

			if body.IsNull() || !body.IsKnown() {
				continue
			}

			var source string
			var constraintVersion string
			if body.Type().IsObjectType() {
				// v0.13 required provider definition
				constraintDef := body.AsValueMap()
				_ = gocty.FromCtyValue(constraintDef["source"], &source)
				if source != "hashicorp/aws" {
					continue
				}

				_ = gocty.FromCtyValue(constraintDef["version"], &constraintVersion)
			} else {
				// support v0.12 provider definition
				// https://developer.hashicorp.com/terraform/language/providers/requirements#v0-12-compatible-provider-requirements
				_ = gocty.FromCtyValue(body, &constraintVersion)
			}

			// parse the version and return it
			constraints, err := version.NewConstraint(constraintVersion)
			if err == nil {
				return &ProviderConstraints{AWS: constraints}
			}
		}
	}

	return nil
}
