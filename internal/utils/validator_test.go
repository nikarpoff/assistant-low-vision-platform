package utils

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/logger"
)

type testRequest struct {
	IDs   []string `json:"ids" validate:"required,min=1"`
	Limit int      `json:"limit,omitempty" validate:"gte=0"`
}

func TestValidator_Validate(t *testing.T) {
	var validator *Validator

	setup := func() {
		logger.InitTestLogger()
		validator = NewValidator().WithJsonFieldNames()
	}

	t.Run("must_fail_with_json_field_names", func(t *testing.T) {
		setup()

		result := validator.Validate(testRequest{Limit: -1})

		require.Error(t, result.Error)
		require.Equal(t, errValidation+" for fields: ids, limit", result.Message)
	})

	t.Run("must_pass", func(t *testing.T) {
		setup()

		result := validator.Validate(testRequest{IDs: []string{"id-1"}})

		require.NoError(t, result.Error)
		require.Empty(t, result.Message)
	})
}
