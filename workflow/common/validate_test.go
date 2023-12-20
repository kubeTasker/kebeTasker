package common

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

type param struct {
	Name string
}

func TestValidateworkflowFieldNames(t *testing.T) {
	tests := map[string]struct {
		Names       []param
		ExpectedErr error
	}{
		"no name": {
			Names:       []param{param{""}},
			ExpectedErr: fmt.Errorf("[0].name is required"),
		},
		"invalid workflow field name length": {
			Names:       []param{param{"this-is-a-super-long-template-name-this-is-a-super-long-template-name-this-is-a-super-long-template-name-this-is-a-super-long-template-name"}},
			ExpectedErr: fmt.Errorf("[0].name: 'this-is-a-super-long-template-name-this-is-a-super-long-template-name-this-is-a-super-long-template-name-this-is-a-super-long-template-name' is invalid: must be no more than 128 characters"),
		},
		"invalid filed name": {
			Names:       []param{param{"-whalesay"}},
			ExpectedErr: fmt.Errorf("[0].name: '-whalesay' is invalid: name must consist of alpha-numeric characters or '-', and must start with an alpha-numeric character(e.g. My-name1-2, 123-NAME)"),
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			err := validateWorkflowFieldNames(tc.Names)
			if tc.ExpectedErr != nil {
				assert.EqualError(t, err, tc.ExpectedErr.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
